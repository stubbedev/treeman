// Package adapter holds the small, engine-agnostic helpers shared by
// the per-engine driver packages. Kept thin on purpose — anything
// engine-specific (DSN shape, snapshot strategy, identifier quoting)
// lives in its own package.
package adapter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Await runs connect once, or, under a ctx marked by
// reachability.WithStartupWait, until it succeeds, fails for a reason
// other than the engine being unreachable (reachability.IsUnreachable:
// refused, timed out, its container down, the handshake dropped, the
// engine still starting), or reachability.StartupWait elapses. A
// failure the engine itself reports (authentication, an unknown
// database) returns at once.
func Await[T any](ctx context.Context, connect func(context.Context) (T, error)) (T, error) {
	var out T
	err := reachability.WaitFor(ctx, func(ctx context.Context) error {
		v, err := connect(ctx)
		out = v
		return err
	}, reachability.IsUnreachable)
	return out, err
}

// Connect connects to engine at host:port, or at the address of the
// container opts names (Container or ComposeService), the way Await
// retries: each attempt resolves the container, probes the endpoint
// over TCP, then runs connect against the resolved address. An attempt
// whose probe fails against a containerized target refreshes the
// container-IP cache and probes the re-resolved address once more, so
// a container that restarted with a new IP or port settles within one
// attempt.
func Connect[T any](
	ctx context.Context,
	engine string,
	opts containerip.Opts,
	host string,
	port uint16,
	connect func(ctx context.Context, host string, port uint16) (T, error),
) (T, error) {
	return Await(ctx, func(ctx context.Context) (T, error) {
		h, p, err := resolveAndProbeOnce(ctx, engine, opts, host, port)
		if err != nil {
			var zero T
			return zero, err
		}
		return connect(ctx, h, p)
	})
}

// ConnectURL is Connect for a URL-style connection string: connect
// gets uri with its host and port rewritten to the resolved container
// address (uri unchanged when opts names no container), and that
// address as host:port for error messages.
func ConnectURL[T any](
	ctx context.Context,
	engine string,
	opts containerip.Opts,
	uri string,
	connect func(ctx context.Context, uri, addr string) (T, error),
) (T, error) {
	host, port, err := reachability.HostPort(engine, uri)
	if err != nil {
		var zero T
		return zero, err
	}
	return Connect(ctx, engine, opts, host, port, func(ctx context.Context, h string, p uint16) (T, error) {
		resolved := uri
		if h != host || p != port {
			resolved = containerip.RewriteHostPortInURIWithPort(uri, h, p)
		}
		return connect(ctx, resolved, net.JoinHostPort(h, strconv.Itoa(int(p))))
	})
}

func resolveAndProbeOnce(ctx context.Context, engine string, opts containerip.Opts, host string, port uint16) (string, uint16, error) {
	h, p, err := resolveEndpoint(ctx, engine, opts, host, port)
	if err != nil {
		return host, port, err
	}
	probeErr := reachability.ProbeCtx(ctx, engine, h, p)
	if probeErr == nil || (opts.Container == "" && opts.ComposeService == "") {
		return h, p, probeErr
	}
	containerip.RefreshOpts(opts)
	h2, p2, err := resolveEndpoint(ctx, engine, opts, host, port)
	if err != nil {
		return h, p, fmt.Errorf("%w (re-resolving the container after that also failed: %w)", probeErr, err)
	}
	return h2, p2, reachability.ProbeCtx(ctx, engine, h2, p2)
}

// resolveEndpoint returns the address of the container opts names, or
// host:port when it names none. A container that is stopped or not
// created yet is an engine that is down: an *UnreachableError.
func resolveEndpoint(ctx context.Context, engine string, opts containerip.Opts, host string, port uint16) (string, uint16, error) {
	addr, err := containerip.ResolveAddr(ctx, opts)
	if nr, ok := errors.AsType[*containerip.NotRunningError](err); ok {
		return "", 0, &reachability.UnreachableError{Engine: engine, Addr: nr.Ref, Reason: reachability.ContainerDown, Err: err}
	}
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

// AppliedBatches reads a framework's batch ledger table through db:
// countColumns must count the table's name and batch columns (2 when
// the table exists with both), selectRows must return its (name, batch)
// rows. found is false when the table or one of its columns is absent.
// The ledger holds one row per applied migration, so the read is
// bounded by the migration count.
func AppliedBatches(
	ctx context.Context,
	db *sql.DB,
	countColumns string,
	countArgs []any,
	selectRows string,
) (batches map[string]int, found bool, err error) {
	var cols int
	if err := db.QueryRowContext(ctx, countColumns, countArgs...).Scan(&cols); err != nil {
		return nil, false, fmt.Errorf("look up the migration ledger table: %w", err)
	}
	if cols != 2 {
		return nil, false, nil
	}
	rows, err := db.QueryContext(ctx, selectRows)
	if err != nil {
		return nil, false, fmt.Errorf("read the migration ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()
	batches = map[string]int{}
	for rows.Next() {
		var (
			name  string
			batch int
		)
		if err := rows.Scan(&name, &batch); err != nil {
			return nil, false, fmt.Errorf("read the migration ledger: %w", err)
		}
		if _, dup := batches[name]; dup {
			return nil, false, fmt.Errorf("the migration ledger records %q twice", name)
		}
		batches[name] = batch
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read the migration ledger: %w", err)
	}
	return batches, true, nil
}
