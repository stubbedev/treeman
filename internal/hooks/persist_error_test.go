package hooks

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

func TestPersistOutcomeReportsHookRunsItCouldNotRecord(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/a", "a", "main")
	if _, err := s.DB.ExecContext(ctx, "DROP TABLE hook_runs"); err != nil {
		t.Fatal(err)
	}

	PersistOutcome(ctx, s, repoID, wtID, CreateBeforeEngines, 1000, 1500, RunOutcome{
		Groups: []GroupOutcome{{Command: "echo hi", ExitCode: 0}},
	})

	events, err := s.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtHooksEnd}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("hooks:end events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Level != store.LevelWarn || !strings.Contains(ev.Message, "not recorded") || !strings.Contains(ev.PayloadJSON, "persist_error") {
		t.Fatalf("a hook run that could not be recorded must be reported on hooks:end, got %+v", ev)
	}
}
