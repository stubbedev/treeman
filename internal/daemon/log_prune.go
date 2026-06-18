package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/stubbedev/treeman/internal/resolve"
)

// LogPruneLoop runs the daemon's retention sweep over the events,
// hook_runs, and hook_log_chunks tables. Wakes on a fixed interval
// and drops every row older than `logs.keep_days` (resolved from the
// global config). `keep_days <= 0` disables the sweep — callers that
// want to keep logs forever set the value negative.
//
// The interval is short relative to the retention window because
// SQLite DELETE is cheap and a stale row sitting around a few extra
// minutes never matters; the goal is "bounded growth", not "exactly
// keep_days".
func LogPruneLoop(ctx context.Context, st *State) {
	const interval = 1 * time.Hour
	t := time.NewTicker(interval)
	defer t.Stop()
	slog.Info("log_prune_loop started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runLogPrune(ctx, st)
		}
	}
}

func runLogPrune(ctx context.Context, st *State) {
	cfg, err := resolve.LoadResolved("")
	if err != nil {
		slog.Warn("log_prune load global cfg", "err", err)
		return
	}
	keepDays := cfg.Logs.KeepDays
	if keepDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).UnixMilli()
	removed, err := st.Store.PruneOldLogs(ctx, cutoff)
	if err != nil {
		slog.Warn("log_prune", "err", err)
		return
	}
	if removed > 0 {
		slog.Info("log_prune swept", "keep_days", keepDays, "rows_removed", removed)
	}

	// Same retention window reaps the path-keyed hash caches, which have
	// no FK to cascade on teardown and otherwise grow unbounded as
	// worktrees come and go (a live path's row is re-touched on every
	// fingerprint scan, so an aged-out row is a dead path).
	hashRows, err := st.Store.PruneStaleHashCaches(ctx, cutoff)
	if err != nil {
		slog.Warn("hash_cache_prune", "err", err)
		return
	}
	if hashRows > 0 {
		slog.Info("hash_cache_prune swept", "keep_days", keepDays, "rows_removed", hashRows)
	}
}
