package mcp

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// rootHeaders are the request headers a proxy or harness may set to hand
// the server a workspace root without the MCP roots round-trip. Values are
// `file://` URIs or plain filesystem paths; multiple roots may be
// comma-separated. Listed in precedence order — the first header that
// yields a value wins.
var rootHeaders = []string{"X-Repo-Root", "X-Mcp-Roots", "X-Mcp-Root", "Mcp-Roots", "Mcp-Root"}

// reqResolver carries the per-request workspace-root context for one MCP
// message. The receiving middleware (installContextMiddleware) builds one
// per incoming request and stashes it in the call's context.Context;
// resolveRepo / resolveWorktree consult it before falling back to the
// process cwd.
//
// This is what lets a single server instance serve many concurrent
// clients over HTTP: each request is pinned to its own worktree via
// headers or the client's MCP roots, with no shared mutable cwd. The
// stdio transport sets no resolver, so resolveRepo keeps its historical
// os.Getwd() behaviour there.
//
// Precedence (highest first): explicit tool param > HTTP header > MCP
// client roots (roots/list) > os.Getwd(). Header roots are authoritative
// over roots/list — a proxy that pins a root means it.
type reqResolver struct {
	headerRoots []string        // filesystem paths parsed from request headers
	listRoots   func() []string // lazy roots/list against this session; nil when the client doesn't support roots
	httpMode    bool            // request arrived over HTTP (no meaningful process cwd to fall back to)

	once   sync.Once // memoizes the resolved primary root for this request
	cached string
}

type ctxKey int

const resolverKey ctxKey = iota

func withResolver(ctx context.Context, r *reqResolver) context.Context {
	return context.WithValue(ctx, resolverKey, r)
}

func resolverFrom(ctx context.Context) *reqResolver {
	r, _ := ctx.Value(resolverKey).(*reqResolver)
	return r
}

// rootDir returns the request's primary workspace root as a filesystem
// path, or "" when none is available. Header roots win over roots/list.
// The result is resolved at most once per request: a single tool call
// resolves both repo and worktree, so without memoization the roots/list
// server->client RPC would fire twice (or more) per call.
func (r *reqResolver) rootDir() string {
	if r == nil {
		return ""
	}
	r.once.Do(func() {
		if len(r.headerRoots) > 0 {
			r.cached = r.headerRoots[0]
			return
		}
		if r.listRoots != nil {
			if roots := r.listRoots(); len(roots) > 0 {
				r.cached = roots[0]
			}
		}
	})
	return r.cached
}

// rootDirFromCtx is the convenience the resolvers use: the request's
// primary root, or "" when running over stdio / no root was supplied.
func rootDirFromCtx(ctx context.Context) string {
	return resolverFrom(ctx).rootDir()
}

// rootPath converts an MCP root value (`file://` URI or plain path) to an
// absolute filesystem path. Empty/whitespace input — or a non-absolute
// path — yields "": in the shared-daemon model a relative root would be
// resolved against the daemon's cwd, not the client's, which is silently
// wrong, so such values are rejected rather than guessed.
func rootPath(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "file://") {
		if u, err := url.Parse(v); err == nil && u.Path != "" {
			v = u.Path
		}
	}
	if !filepath.IsAbs(v) {
		return ""
	}
	return v
}

// parseRootHeaders collects filesystem roots from the recognized request
// headers, preserving header precedence and splitting comma-separated
// values.
func parseRootHeaders(h http.Header) []string {
	if h == nil {
		return nil
	}
	var roots []string
	for _, name := range rootHeaders {
		for _, v := range h.Values(name) {
			for part := range strings.SplitSeq(v, ",") {
				if p := rootPath(part); p != "" {
					roots = append(roots, p)
				}
			}
		}
	}
	return roots
}

// installContextMiddleware wires a receiving middleware that, for every
// incoming request, derives the workspace-root context (HTTP headers +
// the client's MCP roots) and stashes a reqResolver in the call context.
// Tool / resource / prompt handlers then resolve their repo and worktree
// per request rather than from a single process-wide cwd.
func installContextMiddleware(srv *mcpsdk.Server) {
	srv.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			r := &reqResolver{}

			// HTTP headers (when served over the streamable HTTP
			// transport) are authoritative and need no round-trip. Extra
			// is non-nil only on the HTTP transport (stdio leaves it nil),
			// so it also marks that there is no process cwd worth falling
			// back to — see requestCwd.
			if extra := req.GetExtra(); extra != nil {
				r.httpMode = true
				r.headerRoots = parseRootHeaders(extra.Header)
			}

			// Otherwise fall back to the client's MCP roots, fetched
			// lazily and at most once per request. roots/list is a
			// server->client RPC: only wire it when the client actually
			// advertised the roots capability, else ListRoots would send
			// a request the client never answers and block the tool call
			// (the SDK's checkInitialized is a no-op, so it won't reject).
			if ss, ok := req.GetSession().(*mcpsdk.ServerSession); ok && clientSupportsRoots(ss) {
				r.listRoots = func() []string {
					res, err := ss.ListRoots(ctx, nil)
					if err != nil || res == nil {
						return nil
					}
					out := make([]string, 0, len(res.Roots))
					for _, root := range res.Roots {
						if root == nil {
							continue
						}
						if p := rootPath(root.URI); p != "" {
							out = append(out, p)
						}
					}
					return out
				}
			}

			return next(withResolver(ctx, r), method, req)
		}
	})
}

// clientSupportsRoots reports whether the client advertised the MCP roots
// capability at initialize. Guards the server->client roots/list call so
// it is only issued to clients that will answer it.
func clientSupportsRoots(ss *mcpsdk.ServerSession) bool {
	if ss == nil {
		return false
	}
	ip := ss.InitializeParams()
	return ip != nil && ip.Capabilities != nil && ip.Capabilities.RootsV2 != nil
}
