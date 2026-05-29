package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
)

// finalizeTimeout caps how long any single FinalizeWorktree run is
// allowed to occupy its slot before the watchdog cancels it and
// emits a wt_finalize error event. 30 minutes is comfortably above
// every realistic cold-build we've measured (largest production
// schema with seed data: ~12 min) while still catching the silent-
// stall class from issue #9 within the same workday. Not yet
// user-configurable — promote to DaemonConfig if a legitimate need
// arises.
const finalizeTimeout = 30 * time.Minute

// watchdogInterval bounds detection latency. A finalize that wedges
// at minute 25 (5min under the cap) is detected within 2 minutes —
// still well inside the human-noticing window the issue described.
const watchdogInterval = 2 * time.Minute

// SweepStalePreparing fails-out any worktree row whose most recent
// finalize event is `wt_finalize_start` — by definition, no goroutine
// owns that work anymore once the daemon has restarted. Without this
// sweep, `treeman worktree list` keeps reporting `state=preparing`
// indefinitely (the state is derived from the latest finalize event
// per worktree). See issue #9.
//
// The fix is intentionally conservative: emit a synthetic
// `wt_finalize` error event so the derived state flips to `error`
// with a clear message. We don't auto-requeue — silently re-running
// setup hooks + cold builds on a fresh daemon would be surprising,
// and the user already has `treeman wt finalize` to retry. They get a
// visible signal instead of an invisible wedge.
//
// Idempotent: the sweep emits at most one stale event per row per
// daemon start. A second start without intervening activity still
// sees `wt_finalize_start` as the latest finalize event (the error
// event we wrote uses a different event_type) — that's deliberate;
// the message includes the wt_finalize_start timestamp so consecutive
// stale entries don't lose information.
func SweepStalePreparing(ctx context.Context, st *State) {
	wts, err := st.Store.ListActiveWorktrees(ctx)
	if err != nil {
		slog.Warn("stale-preparing sweep: list worktrees", "err", err)
		return
	}
	for _, w := range wts {
		row, err := lookupWorktreeByPath(ctx, st.Store, w.WorktreePath)
		if err != nil || row.ID == 0 {
			continue
		}
		evs, err := st.Store.QueryEvents(ctx, store.EventFilter{
			WorktreeID: row.ID,
			EventTypes: []string{"wt_finalize_start", "wt_finalize_done", "wt_finalize", "wt_finalize_cancelled"},
			Limit:      1,
		})
		if err != nil || len(evs) == 0 {
			continue
		}
		last := evs[0]
		if last.EventType != "wt_finalize_start" {
			continue
		}
		_ = st.Store.WriteEvent(ctx, store.LevelError, "wt_finalize",
			"stale finalize: daemon restarted while prepare was in flight — rerun `treeman wt finalize` to retry",
			row.RepoID, row.ID, "", 0, map[string]any{
				"reason":         "daemon_restart",
				"start_event_ts": last.Ts,
			})
		slog.Warn("marked stuck finalize as stale",
			"wt", w.WorktreePath, "started_ms", last.Ts)

		// Auto-recover the engine state. A prepare killed mid-migrate
		// leaves the active DB (branch_scoped) or the source DB
		// (test-clone) half-migrated; a manual rerun without recovery
		// trips on duplicate-column / table-exists errors. Drop the
		// primary namespace now so the next `treeman wt finalize`
		// re-runs cold-build / branch_scoped seed end-to-end. Durable
		// per-branch copies are preserved (see prepare.RecoverStale-
		// Worktree). Best-effort — config load / engine connect
		// failures are surfaced as `wt_recovery_error` events but
		// don't stop the sweep.
		cfg, err := resolve.LoadResolvedForWorktree(w.RepoPath, w.WorktreePath)
		if err != nil {
			_ = st.Store.WriteEvent(ctx, store.LevelWarn, "wt_recovery_error",
				"load config for recovery: "+err.Error(),
				row.RepoID, row.ID, "", 0, map[string]string{"error": err.Error()})
			continue
		}
		prepare.RecoverStaleWorktree(ctx, &cfg, row.Slug, w.WorktreePath, row.RepoID, row.ID, st.Store)
	}
}

// errString returns err.Error() or "<nil>" when err is nil; lets the
// watchdog recovery branch quote the lookup error inside a Sprintf
// without separate guards.
func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// FinalizeWatchdogLoop is the live-daemon counterpart to
// SweepStalePreparing: it cancels in-flight finalizes that have
// exceeded finalizeTimeout (a child wedged on a network read, a
// blocked DB connection, etc.) and emits a synthetic wt_finalize
// error so the row's derived state flips out of "preparing".
//
// Runs until ctx cancels. Designed to be launched once at boot off
// the daemon-lifetime BgCtx.
//
// The cancel only takes effect at the FinalizeWorktree phase
// boundaries that check ctx.Err() — that's the same contract
// TeardownWorktree relies on, so the timeout doesn't need to model
// any new "force kill" path. If a phase ignores ctx, the row's
// derived state still flips to error because we emit the synthetic
// event regardless of whether the cancel landed.
func FinalizeWatchdogLoop(ctx context.Context, st *State) {
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepExpiredFinalizes(ctx, st)
		}
	}
}

func sweepExpiredFinalizes(ctx context.Context, st *State) {
	cutoff := time.Now().Add(-finalizeTimeout)
	for wtPath, startedAt := range st.SnapshotInFlightFinalizes() {
		if startedAt.After(cutoff) {
			continue
		}
		// Cancel first so the goroutine's next phase-boundary check
		// bails — the in-flight slot will clear when the goroutine
		// returns, but we don't wait. Emitting the synthetic error
		// event below makes the wedge visible even if the goroutine
		// is wedged in a phase that doesn't observe ctx.Err().
		st.CancelFinalize(wtPath)

		row, err := lookupWorktreeByPath(ctx, st.Store, wtPath)
		if err != nil || row.ID == 0 {
			slog.Warn("watchdog: in-flight finalize has no DB row",
				"wt", wtPath, "err", err)
			continue
		}
		age := time.Since(startedAt).Round(time.Second)
		_ = st.Store.WriteEvent(ctx, store.LevelError, "wt_finalize",
			"prepare timed out after "+age.String()+" — child wedged; rerun `treeman wt finalize` once unstuck",
			row.RepoID, row.ID, "", 0, map[string]any{
				"reason":     "prepare_timeout",
				"started_ms": startedAt.UnixMilli(),
				"age_ms":     age.Milliseconds(),
			})
		slog.Warn("watchdog: timed out in-flight finalize",
			"wt", wtPath, "age", age)

		// Same recovery as SweepStalePreparing: a wedged prepare that
		// the watchdog cancels leaves the same half-applied engine
		// state a daemon-restart leaves. Reset primary namespaces so
		// the user's rerun starts clean.
		repoPath, repoErr := st.Store.RepoPath(ctx, row.RepoID)
		if repoErr != nil || repoPath == "" {
			_ = st.Store.WriteEvent(ctx, store.LevelWarn, "wt_recovery_error",
				"lookup repo path for recovery: "+errString(repoErr),
				row.RepoID, row.ID, "", 0, map[string]string{"error": errString(repoErr)})
			continue
		}
		cfg, err := resolve.LoadResolvedForWorktree(repoPath, wtPath)
		if err != nil {
			_ = st.Store.WriteEvent(ctx, store.LevelWarn, "wt_recovery_error",
				"load config for recovery: "+err.Error(),
				row.RepoID, row.ID, "", 0, map[string]string{"error": err.Error()})
			continue
		}
		prepare.RecoverStaleWorktree(ctx, &cfg, row.Slug, wtPath, row.RepoID, row.ID, st.Store)
	}
}
