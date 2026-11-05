// Package adapter holds the small, engine-agnostic helpers shared by
// the per-engine driver packages. Kept thin on purpose — anything
// engine-specific (DSN shape, snapshot strategy, identifier quoting)
// lives in its own package.
package adapter

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// ResolveAndProbe rewrites *host / *port via container-IP detection
// (when opts.Container or opts.ComposeService is set) and probes TCP
// reachability with the caller's context as the deadline source. Under
// a ctx marked by reachability.WithStartupWait a refused connect (an
// engine that is down or still starting) is retried for up to
// reachability.StartupWait; any other failure returns immediately.
//
// Each attempt that fails against a containerized target refreshes the
// container-IP cache and probes the re-resolved address once more
// before returning. This covers the "container restarted, IP/port
// shifted" case that otherwise surfaces as a confusing connect-refused
// on the first wt-create after `docker compose restart`.
//
// The host/port pointers must be non-nil and pre-populated with the
// user's configured values; the function overwrites them with the
// resolved address only when it returns nil.
func ResolveAndProbe(ctx context.Context, engine string, opts containerip.Opts, host *string, port *uint16) error {
	h, p, err := awaitEndpoint(ctx, engine, opts, *host, *port)
	if err != nil {
		return err
	}
	*host, *port = h, p
	return nil
}

// ResolveAndProbeURL is ResolveAndProbe for a URL-style connection
// string: it returns uri with its host/port rewritten to the resolved
// container address, or uri unchanged when opts names no container.
func ResolveAndProbeURL(ctx context.Context, engine string, opts containerip.Opts, uri string) (string, error) {
	host, port, err := reachability.HostPort(engine, uri)
	if err != nil {
		return "", err
	}
	h, p, err := awaitEndpoint(ctx, engine, opts, host, port)
	if err != nil {
		return "", err
	}
	if h == host && p == port {
		return uri, nil
	}
	return containerip.RewriteHostPortInURIWithPort(uri, h, p), nil
}

func awaitEndpoint(ctx context.Context, engine string, opts containerip.Opts, host string, port uint16) (string, uint16, error) {
	resolvedHost, resolvedPort := host, port
	err := reachability.WaitFor(ctx, func(ctx context.Context) error {
		h, p, err := resolveAndProbeOnce(ctx, engine, opts, host, port)
		resolvedHost, resolvedPort = h, p
		return err
	}, reachability.IsRefused)
	return resolvedHost, resolvedPort, err
}

func resolveAndProbeOnce(ctx context.Context, engine string, opts containerip.Opts, host string, port uint16) (string, uint16, error) {
	h, p, err := resolveEndpoint(ctx, opts, host, port)
	if err != nil {
		return host, port, err
	}
	probeErr := reachability.ProbeCtx(ctx, engine, h, p)
	if probeErr == nil || (opts.Container == "" && opts.ComposeService == "") {
		return h, p, probeErr
	}
	containerip.RefreshOpts(opts)
	h2, p2, err := resolveEndpoint(ctx, opts, host, port)
	if err != nil {
		return h, p, fmt.Errorf("%w (re-resolving the container after that also failed: %v)", probeErr, err)
	}
	return h2, p2, reachability.ProbeCtx(ctx, engine, h2, p2)
}

func resolveEndpoint(ctx context.Context, opts containerip.Opts, host string, port uint16) (string, uint16, error) {
	addr, err := containerip.ResolveAddr(ctx, opts)
	if err != nil {
		return "", 0, fmt.Errorf("resolve container: %w", err)
	}
	if addr == nil {
		return host, port, nil
	}
	if addr.Port != 0 {
		port = addr.Port
	}
	return addr.Host, port, nil
}

// ConfigurePool applies sensible defaults to a freshly-opened
// database/sql pool. Without these, the stdlib defaults
// MaxIdleConns=2 (forcing connect/disconnect churn) and no max
// lifetime (so connections stay open across server restarts and
// trip "connection reset" errors when the server recycles them).
func ConfigurePool(db *sql.DB, poolMax int) {
	if poolMax > 0 {
		db.SetMaxOpenConns(poolMax)
		idle := max(poolMax/2, 1)
		db.SetMaxIdleConns(idle)
	}
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
}
