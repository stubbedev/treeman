package hooks

import (
	"context"
	"fmt"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

// PersistOutcome writes one hook_runs row per group and emits paired
// hooks:start / hooks:end events for the phase as a whole. Safe to call
// with st == nil or wtID == 0 — both result in a no-op so non-daemon /
// orphan call sites (where store wiring is awkward) can opt in
// gradually.
//
// `startedMs` is the unix-ms instant the caller spawned the first
// group. `finishedMs` is when RunHooks returned (== now for the
// wait=true path). For the wait=false fire-and-forget path callers
// should pass finishedMs=0 and exitCode=-1 per group, signalling
// "spawned, completion unknown."
//
// Returns the hook_run id per group, index-aligned to out.Groups (0 for
// any group whose row write failed), so callers can cite the exact id in
// a `treeman logs hooks --show <id>` pointer. Returns nil on the no-op
// path.
func PersistOutcome(
	ctx context.Context,
	st *store.Store,
	repoID, wtID int64,
	phase string,
	startedMs, finishedMs int64,
	out RunOutcome,
) []int64 {
	if st == nil || wtID == 0 || len(out.Groups) == 0 {
		return nil
	}

	runIDs := make([]int64, len(out.Groups))
	failed := 0
	maxExit := 0
	for i, g := range out.Groups {
		// Every group's merged stdout+stderr is written to a log
		// file; StderrTail holds the last few KB of that log and is
		// populated only when the group exited non-zero. We record
		// the log path in the command column so the caller can still
		// reach the full bytes if needed.
		cmd := g.Command
		if g.LogPath != "" {
			cmd = fmt.Sprintf("%s  # log=%s", cmd, g.LogPath)
		}
		runID, _ := st.WriteHookRun(ctx, wtID, phase, i, cmd,
			startedMs, finishedMs, g.ExitCode, g.StdoutTail, g.StderrTail)
		runIDs[i] = runID
		// Stash the merged stdout+stderr capture so the failure (or
		// success) survives worktree teardown. ANSI escapes are
		// preserved verbatim — `treeman logs hooks show <id>` writes
		// the bytes back to stdout so terminal color round-trips.
		if runID > 0 && len(g.LogBody) > 0 {
			_ = st.AppendHookLogChunk(ctx, runID, "merged", g.LogBody)
		}
		if g.ExitCode != 0 {
			failed++
			if g.ExitCode > maxExit {
				maxExit = g.ExitCode
			}
		}
	}

	level := store.LevelInfo
	if failed > 0 {
		level = store.LevelError
	}

	dur := int64(0)
	if finishedMs > startedMs {
		dur = finishedMs - startedMs
	}

	payload := map[string]any{
		"groups":   len(out.Groups),
		"failed":   failed,
		"max_exit": maxExit,
	}
	if out.AggregateExitCode != 0 {
		payload["aggregate_exit_code"] = out.AggregateExitCode
	}

	msg := fmt.Sprintf("groups=%d failed=%d", len(out.Groups), failed)
	_ = st.WriteEvent(ctx, level, store.EvtHooksEnd, msg, repoID, wtID, phase, dur, payload)
	return runIDs
}

// EmitHookStart writes a hooks:start event so `treeman logs tail` shows
// the phase began even when every group succeeds and the only signal
// would otherwise be a single hooks:end. Safe to call with st == nil.
func EmitHookStart(ctx context.Context, st *store.Store, repoID, wtID int64, phase string, entryCount int) int64 {
	now := time.Now().UnixMilli()
	if st == nil || wtID == 0 {
		return now
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtHooksStart,
		fmt.Sprintf("groups=%d", entryCount),
		repoID, wtID, phase, 0,
		map[string]any{"groups": entryCount})
	return now
}
