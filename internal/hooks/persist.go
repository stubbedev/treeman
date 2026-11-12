package hooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	phase Trigger,
	startedMs, finishedMs int64,
	out RunOutcome,
) []int64 {
	if st == nil || wtID == 0 || len(out.Groups) == 0 {
		return nil
	}

	runIDs := make([]int64, len(out.Groups))
	var persistErrs []error
	failed := 0
	nonFatal := 0
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
		runID, err := persistGroup(ctx, st, wtID, phase, i, cmd, startedMs, finishedMs, g)
		if err != nil {
			persistErrs = append(persistErrs, err)
		}
		runIDs[i] = runID
		if g.ExitCode != 0 {
			failed++
			if g.ContinueOnError {
				nonFatal++
			}
			if g.ExitCode > maxExit {
				maxExit = g.ExitCode
			}
		}
	}

	level := store.LevelInfo
	if failed > 0 {
		level = store.LevelWarn
		if failed > nonFatal {
			level = store.LevelError
		}
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
	if persistErr := errors.Join(persistErrs...); persistErr != nil {
		slog.Error("hook runs not recorded", "phase", phase.String(), "worktree_id", wtID, "err", persistErr)
		payload["persist_error"] = persistErr.Error()
		msg += "; hook runs not recorded: " + persistErr.Error()
		if level == store.LevelInfo {
			level = store.LevelWarn
		}
	}
	if nonFatal > 0 {
		payload["non_fatal"] = nonFatal
		msg += fmt.Sprintf(" non-fatal=%d", nonFatal)
		if failed == nonFatal {
			msg += " (continuing)"
		}
	}
	_ = st.WriteEvent(ctx, level, store.EvtHooksEnd, msg, repoID, wtID, phase.String(), dur, payload)
	return runIDs
}

func persistGroup(
	ctx context.Context,
	st *store.Store,
	wtID int64,
	phase Trigger,
	i int,
	cmd string,
	startedMs, finishedMs int64,
	g GroupOutcome,
) (int64, error) {
	runID, err := st.WriteHookRun(ctx, wtID, phase.String(), i, cmd,
		startedMs, finishedMs, g.ExitCode, g.StdoutTail, g.StderrTail)
	if err != nil {
		return 0, fmt.Errorf("record %s hook group %d (%s): %w", phase, i, g.Command, err)
	}
	if len(g.LogBody) > 0 {
		if err := st.AppendHookLogChunk(ctx, runID, "merged", g.LogBody); err != nil {
			return runID, fmt.Errorf("store the log of %s hook run %d: %w", phase, runID, err)
		}
	}
	return runID, nil
}

// EmitHookStart writes a hooks:start event so `treeman logs tail` shows
// the phase began even when every group succeeds and the only signal
// would otherwise be a single hooks:end. Safe to call with st == nil.
func EmitHookStart(ctx context.Context, st *store.Store, repoID, wtID int64, phase Trigger, entryCount int) int64 {
	now := time.Now().UnixMilli()
	if st == nil || wtID == 0 {
		return now
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtHooksStart,
		fmt.Sprintf("groups=%d", entryCount),
		repoID, wtID, phase.String(), 0,
		map[string]any{"groups": entryCount})
	return now
}
