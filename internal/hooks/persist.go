package hooks

import (
	"context"
	"fmt"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

// PersistOutcome writes one hook_runs row per group and emits paired
// hook_start / hook_done events for the phase as a whole. Safe to call
// with st == nil or wtID == 0 — both result in a no-op so non-daemon /
// orphan call sites (where store wiring is awkward) can opt in
// gradually.
//
// `startedMs` is the unix-ms instant the caller spawned the first
// group. `finishedMs` is when RunHooks returned (== now for the
// wait=true path). For the wait=false fire-and-forget path callers
// should pass finishedMs=0 and exitCode=-1 per group, signalling
// "spawned, completion unknown."
func PersistOutcome(
	ctx context.Context,
	st *store.Store,
	repoID, wtID int64,
	phase string,
	startedMs, finishedMs int64,
	out RunOutcome,
) {
	if st == nil || wtID == 0 || len(out.Groups) == 0 {
		return
	}

	failed := 0
	maxExit := 0
	for i, g := range out.Groups {
		// stdout/stderr tails are only captured by runForeground
		// (precreate). The async groups dump everything to a log file
		// — we record the path in the command column so the caller
		// can still reach the bytes if needed.
		cmd := g.Command
		if g.LogPath != "" {
			cmd = fmt.Sprintf("%s  # log=%s", cmd, g.LogPath)
		}
		_ = st.WriteHookRun(ctx, wtID, phase, i, cmd,
			startedMs, finishedMs, g.ExitCode, g.StdoutTail, g.StderrTail)
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
	_ = st.WriteEvent(ctx, level, "hook_done", msg, repoID, wtID, phase, dur, payload)
}

// EmitHookStart writes a hook_start event so `treeman logs tail` shows
// the phase began even when every group succeeds and the only signal
// would otherwise be a single hook_done. Safe to call with st == nil.
func EmitHookStart(ctx context.Context, st *store.Store, repoID, wtID int64, phase string, entryCount int) int64 {
	now := time.Now().UnixMilli()
	if st == nil || wtID == 0 {
		return now
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, "hook_start",
		fmt.Sprintf("groups=%d", entryCount),
		repoID, wtID, phase, 0,
		map[string]any{"groups": entryCount})
	return now
}
