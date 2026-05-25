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
// reachability with the caller's context as the deadline source.
//
// On probe failure against a containerized target, the container-IP
// cache is refreshed and one more probe is attempted before
// returning. This covers the "container restarted, IP/port shifted"
// case that otherwise surfaces as a confusing connect-refused on the
// first wt-create after `docker compose restart`.
//
// The host/port pointers must be non-nil and pre-populated with the
// user's configured values; the function only overwrites them when
// container-IP resolution succeeds.
func ResolveAndProbe(ctx context.Context, engine string, opts containerip.Opts, host *string, port *uint16) error {
	if addr, err := containerip.ResolveAddr(opts); err != nil {
		return fmt.Errorf("resolve container: %w", err)
	} else if addr != nil {
		*host = addr.Host
		if addr.Port != 0 {
			*port = addr.Port
		}
	}
	if err := reachability.ProbeCtx(ctx, engine, *host, *port); err != nil {
		if opts.Container == "" && opts.ComposeService == "" {
			return err
		}
		containerip.RefreshOpts(opts)
		addr, e := containerip.ResolveAddr(opts)
		if e != nil || addr == nil {
			return err
		}
		*host = addr.Host
		if addr.Port != 0 {
			*port = addr.Port
		}
		if err2 := reachability.ProbeCtx(ctx, engine, *host, *port); err2 != nil {
			return err2
		}
	}
	return nil
}

// ConfigurePool applies sensible defaults to a freshly-opened
// database/sql pool. Without these, the stdlib defaults
// MaxIdleConns=2 (forcing connect/disconnect churn) and no max
// lifetime (so connections stay open across server restarts and
// trip "connection reset" errors when the server recycles them).
func ConfigurePool(db *sql.DB, poolMax int) {
	if poolMax > 0 {
		db.SetMaxOpenConns(poolMax)
		idle := poolMax / 2
		if idle < 1 {
			idle = 1
		}
		db.SetMaxIdleConns(idle)
	}
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
}
