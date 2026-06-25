package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Default HTTP bind address and endpoint path. The host is loopback so the
// server is not inadvertently exposed; an explicit non-loopback host listens
// on all interfaces but disables DNS-rebinding protection and ships an
// unauthenticated RCE surface — see the ServeHTTP doc before doing so. The
// port avoids sentry-mcp's 8765 so both can run side by side.
const (
	defaultHTTPAddr = "127.0.0.1:8787"
	defaultHTTPPath = "/mcp"
)

// HTTPConfig inspects the `treeman mcp` argument list (and the
// TREEMAN_MCP_HTTP* env vars) for HTTP-mode configuration. It returns a
// non-empty addr when the HTTP transport is requested; an empty addr means
// the caller should use the default stdio transport.
//
// HTTP mode is enabled by --http (optionally --http=addr or --http addr) or
// by setting TREEMAN_MCP_HTTP_ADDR; a bare --http, or a truthy
// TREEMAN_MCP_HTTP, binds the loopback default. The endpoint path comes from
// --http-path / --http-path=… / TREEMAN_MCP_HTTP_PATH, defaulting to /mcp.
//
// Args are parsed manually because `treeman mcp` sets SkipFlagParsing so
// that MCP clients passing flags meant for other servers don't wedge the
// handshake.
func HTTPConfig(args []string) (addr, path string) {
	path = defaultHTTPPath
	for i, a := range args {
		switch {
		case a == "--http":
			addr = defaultHTTPAddr
			// Accept a following token only when it is unambiguously a
			// host:port bind. SkipFlagParsing means MCP clients inject
			// foreign flags/values here, so a loose "contains a colon"
			// test would swallow e.g. an unrelated "key:value" arg.
			if i+1 < len(args) && looksLikeListenAddr(args[i+1]) {
				addr = args[i+1]
			}
		case strings.HasPrefix(a, "--http="):
			if v := strings.TrimPrefix(a, "--http="); v != "" {
				addr = v
			} else {
				addr = defaultHTTPAddr
			}
		case a == "--http-path":
			if i+1 < len(args) {
				path = ensureLeadingSlash(args[i+1])
			}
		case strings.HasPrefix(a, "--http-path="):
			path = ensureLeadingSlash(strings.TrimPrefix(a, "--http-path="))
		}
	}

	if addr == "" {
		if v := os.Getenv("TREEMAN_MCP_HTTP_ADDR"); v != "" {
			addr = v
		} else if v := strings.ToLower(os.Getenv("TREEMAN_MCP_HTTP")); v == "1" || v == "true" || v == "yes" {
			addr = defaultHTTPAddr
		}
	}
	if p := os.Getenv("TREEMAN_MCP_HTTP_PATH"); p != "" && !pathFlagGiven(args) {
		path = ensureLeadingSlash(p)
	}
	return addr, path
}

func pathFlagGiven(args []string) bool {
	for _, a := range args {
		if a == "--http-path" || strings.HasPrefix(a, "--http-path=") {
			return true
		}
	}
	return false
}

// looksLikeListenAddr reports whether s is a "host:port" / ":port" bind
// with a numeric port — the only shape accepted as the bare --http value.
func looksLikeListenAddr(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	_, port, err := net.SplitHostPort(s)
	if err != nil || port == "" {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// addrIsLoopback reports whether addr binds a loopback (or unspecified-host
// shorthand like ":8787", which net/http treats as all interfaces — callers
// treat only an explicit loopback host as safe). Used to warn on exposure.
func addrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		// ":8787" / no host → all interfaces; not loopback.
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

func ensureLeadingSlash(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return defaultHTTPPath
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// ServeHTTP boots the MCP server on the SDK's streamable HTTP transport,
// binding addr and serving the protocol at path. It blocks until ctx is
// cancelled (graceful shutdown) or the listener fails.
//
// One server instance serves every client: the getServer closure hands the
// same *Server to each incoming request. Per-request workspace context is
// derived from request headers (X-Repo-Root / X-Mcp-Root…) or the client's
// MCP roots by installContextMiddleware — so a single shared daemon can
// back every Claude Code instance, each pinned to its own worktree, with no
// process-wide cwd.
//
// SECURITY: there is NO authentication on the endpoint, and treeman's tool
// surface runs shell hooks, prepare scripts and arbitrary DB queries — i.e.
// reaching the port is equivalent to local code execution. The default bind
// is loopback, where the SDK's DNS-rebinding protection is active. Binding a
// non-loopback address (e.g. 0.0.0.0) DISABLES that protection (the SDK only
// guards loopback listeners) and exposes that RCE surface unauthenticated:
// only do so behind a reverse proxy that terminates TLS and authenticates,
// on a trusted network. ServeHTTP logs a warning when bound non-loopback.
func ServeHTTP(ctx context.Context, addr, path string) error {
	if !addrIsLoopback(addr) {
		fmt.Fprintf(
			os.Stderr,
			"treeman mcp: WARNING binding non-loopback %q with no auth; treeman tools run shell/DB commands — front with an authenticating reverse proxy on a trusted network only\n",
			addr,
		)
	}
	srv := newServer()
	handler := mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{
			// Reap sessions abandoned by a client that exited without a
			// DELETE, so a long-lived shared daemon doesn't leak them.
			SessionTimeout: 30 * time.Minute,
		},
	)

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return fmt.Errorf("mcp http server: %w", err)
	}
}
