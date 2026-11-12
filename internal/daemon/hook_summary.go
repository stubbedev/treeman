package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/store"
)

type hookSummaryKey struct{}

const (
	finalizeCompleteMessage       = "daemon-detached setup + prepare complete"
	watchRePrepareCompleteMessage = "watch re-prepare complete"
)

type hookWarning struct {
	Phase    hooks.Trigger `json:"phase"`
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"`
	RunID    int64         `json:"run_id"`
}

type hookSummary struct {
	warnings []hookWarning
}

func (s *hookSummary) record(phase hooks.Trigger, out hooks.RunOutcome, runIDs []int64) {
	for i, g := range out.Groups {
		if g.ExitCode == 0 || !g.ContinueOnError {
			continue
		}
		w := hookWarning{Phase: phase, Command: g.Command, ExitCode: g.ExitCode}
		if i < len(runIDs) {
			w.RunID = runIDs[i]
		}
		s.warnings = append(s.warnings, w)
	}
}

func (s *hookSummary) emit(ctx context.Context, st *store.Store, repoID, wtID int64, message string) {
	level := store.LevelInfo
	var payload map[string]any
	if len(s.warnings) > 0 {
		level = store.LevelWarn
		details := make([]string, 0, len(s.warnings))
		for _, w := range s.warnings {
			detail := fmt.Sprintf("%s: hook exited %d: %s", w.Phase, w.ExitCode, w.Command)
			if w.RunID > 0 {
				detail += fmt.Sprintf(" (treeman logs hooks --all --show %d)", w.RunID)
			}
			details = append(details, detail)
		}
		message += fmt.Sprintf(" with %d non-fatal hook failure(s): %s", len(s.warnings), strings.Join(details, "; "))
		payload = map[string]any{"non_fatal_hooks": s.warnings}
	}
	_ = st.WriteEvent(ctx, level, store.EvtWorktreeCreateEnd, message, repoID, wtID, "", 0, payload)
}
