package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
)

// finalizeTimeout is how long a finalize may go without progress
// before FinalizeWatchdogLoop cancels its work as stalled. The clock
// starts when the finalize holds its cross-process lock, restarts on
// every clone restore admitted to or finishing in its slot, and is
// paused while a restore waits for a daemon-wide clone slot (#199):
// eight concurrent creates queue behind each other's restores, and
// that wait is progress, not a wedge.
//
// 15 minutes sits just above the largest realistic single step we've
// measured (largest production schema with seed data: ~12 min) so a
// legitimate slow build is never aborted, while a silent stall (the
// issue-#9 / ES-meltdown class) stops pinning a slot within ~15 min +
// one watchdog interval.
const finalizeTimeout = 15 * time.Minute

// watchdogInterval bounds detection latency. A finalize that wedges
// at minute 25 (5min under the cap) is detected within 2 minutes —
// still well inside the human-noticing window the issue described.
const watchdogInterval = 2 * time.Minute

// SweepStalePreparing fails-out any worktree row whose most recent
// finalize event is `worktree:create:start` — by definition, no goroutine
// owns that work anymore once the daemon has restarted. That holds only
// because treemand claims the store-wide daemon lock (wtlock.AcquireDaemon)
// before this runs: a second daemon on the same store would otherwise
// fail the first one's live creates (issue #127). Without this
// sweep, `treeman worktree list` keeps reporting `state=preparing`
// indefinitely (the state is derived from the latest finalize event
// per worktree). See issue #9.
//
// The fix is intentionally conservative: emit a synthetic
// `worktree:create:error` error event so the derived state flips to `error`
// with a clear message. We don't auto-requeue — silently re-running
// setup hooks + cold builds on a fresh daemon would be surprising,
// and the user already has `treeman worktree finalize` to retry. They get a
// visible signal instead of an invisible wedge.
//
// Idempotent: the sweep emits at most one stale event per row per
// daemon start. A second start without intervening activity still
// sees `worktree:create:start` as the latest finalize event (the error
// event we wrote uses a different event_type) — that's deliberate;
// the message includes the worktree:create:start timestamp so consecutive
// stale entries don't lose information.
func SweepStalePreparing(ctx context.Context, st *State) {
	wts, err := st.Store.ListActiveWorktreeRows(ctx)
	if err != nil {
		slog.Warn("stale-preparing sweep: list worktrees", "err", err)
		return
	}
	ids := make([]int64, len(wts))
	for i := range wts {
		ids[i] = wts[i].ID
	}
	// One query for every row's latest finalize event — the sweep used
	// to fire two queries per worktree at boot (row lookup + events).
	latest, err := st.Store.LatestEventPerWorktree(ctx, ids, []string{
		store.EvtWorktreeCreateStart,
		store.EvtWorktreeCreateEnd,
		store.EvtWorktreeCreateError,
		store.EvtWorktreeCreateCancel,
	})
	if err != nil {
		slog.Warn("stale-preparing sweep: query events", "err", err)
		return
	}
	for _, row := range wts {
		last, ok := latest[row.ID]
		if !ok || last.EventType != store.EvtWorktreeCreateStart {
			continue
		}
		_ = st.Store.WriteEvent(ctx, store.LevelError, store.EvtWorktreeCreateError,
			"stale finalize: daemon restarted while prepare was in flight — rerun `treeman worktree finalize` to retry",
			row.RepoID, row.ID, "", 0, map[string]any{
				"reason":         "daemon_restart",
				"start_event_ts": last.Ts,
			})
		slog.Warn("marked stuck finalize as stale",
			"wt", row.Path, "started_ms", last.Ts)

		// Auto-recover the engine state. A prepare killed mid-migrate
		// leaves the active DB (branch_scoped) or the source DB
		// (test-clone) half-migrated; a manual rerun without recovery
		// trips on duplicate-column / table-exists errors. Drop the
		// primary namespace now so the next `treeman worktree finalize`
		// re-runs cold-build / branch_scoped seed end-to-end. Durable
		// per-branch copies are preserved (see prepare.RecoverStale-
		// Worktree). Best-effort — config load / engine connect
		// failures are surfaced as `wt_recovery_error` events but
		// don't stop the sweep.
		cfg, err := resolve.LoadResolvedForWorktree(row.RepoPath, row.Path)
		if err != nil {
			_ = st.Store.WriteEvent(ctx, store.LevelWarn, store.EvtWorktreeRecoverError,
				"load config for recovery: "+err.Error(),
				row.RepoID, row.ID, "", 0, map[string]string{"error": err.Error()})
			continue
		}
		prepare.RecoverStaleWorktree(ctx, &cfg, row.Slug, row.Path, row.RepoID, row.ID, st.Store)
	}
}

// FinalizeWatchdogLoop is the live-daemon counterpart to
// SweepStalePreparing: it cancels the work of in-flight finalizes that
// made no progress for finalizeTimeout (a child wedged on a network
// read, a blocked DB connection, etc.).
//
// Runs until ctx cancels. Designed to be launched once at boot off
// the daemon-lifetime BgCtx.
//
// The cancel carries a finalizeStalledError cause. The finalize records
// the failure (and recovers the databases it touched) itself once its
// work returns, so recovery never races a prepare that is still
// running; a step that finishes despite the cancel renews the lease and
// the finalize carries on to `ready`. Only a finalize still in flight a
// watchdog interval after its cancel (a phase that ignores its context)
// gets a worktree:create:error from here, so its state stops reading
// `preparing`; should it still finish, its later worktree:create:end
// supersedes that error.
//
// The same tick re-runs creates that failed for a transient cause once
// their back-off elapsed (see sweepCreateRetries).
func FinalizeWatchdogLoop(ctx context.Context, st *State) {
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepExpiredFinalizes(ctx, st)
			sweepCreateRetries(ctx, st)
		}
	}
}

func sweepExpiredFinalizes(ctx context.Context, st *State) {
	now := time.Now()
	for wtPath, exp := range st.expiredLeases(now) {
		idle := now.Sub(exp.lease.lastProgress).Round(time.Second)
		if exp.action == watchdogStall {
			exp.lease.stall(&finalizeStalledError{idle: idle})
			slog.Warn("watchdog: cancelled stalled finalize", "wt", wtPath, "idle", idle)
			continue
		}
		row, err := lookupWorktreeByPath(ctx, st.Store, wtPath)
		if err != nil || row.ID == 0 {
			slog.Warn("watchdog: in-flight finalize has no DB row",
				"wt", wtPath, "err", err)
			continue
		}
		_ = st.Store.WriteEvent(ctx, store.LevelError, store.EvtWorktreeCreateError,
			"prepare timed out: "+(&finalizeStalledError{idle: idle}).Error()+
				", and it did not stop when cancelled; rerun `treeman worktree finalize` once unstuck",
			row.RepoID, row.ID, "", 0, map[string]any{
				"reason":     "prepare_timeout",
				"started_ms": exp.startedAt.UnixMilli(),
				"idle_ms":    idle.Milliseconds(),
			})
		slog.Warn("watchdog: stalled finalize did not stop", "wt", wtPath, "idle", idle)
	}
}
