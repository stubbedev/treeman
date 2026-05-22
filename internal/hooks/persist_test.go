package hooks

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

// TestPersistOutcomeWritesRowsAndEvent covers the happy path:
// PersistOutcome inserts one hook_runs row per group and emits one
// hook_done event whose payload reflects the group counts.
func TestPersistOutcomeWritesRowsAndEvent(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/a", "a", "main")

	out := RunOutcome{
		Groups: []GroupOutcome{
			{Command: "echo hi", ExitCode: 0, StdoutTail: "hi\n"},
			{Command: "false", ExitCode: 1, StderrTail: "exit 1\n"},
		},
	}
	PersistOutcome(ctx, s, repoID, wtID, "postcreate", 1000, 1500, out)

	runs, err := s.QueryHookRuns(ctx, wtID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d rows want 2", len(runs))
	}
	events, err := s.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{"hook_done"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d hook_done events want 1", len(events))
	}
	if events[0].Level != store.LevelError {
		t.Errorf("expected error level (one group failed); got %s", events[0].Level)
	}
	if events[0].Phase != "postcreate" {
		t.Errorf("phase mismatch: %s", events[0].Phase)
	}
}

// TestPersistOutcomeNoopWhenStoreMissing makes sure call sites with
// no SQLite at hand (e.g. the daemon before store wiring landed) can
// pass nil safely.
func TestPersistOutcomeNoopWhenStoreMissing(t *testing.T) {
	PersistOutcome(context.Background(), nil, 0, 0, "postcreate", 1, 2,
		RunOutcome{Groups: []GroupOutcome{{Command: "noop"}}})
}

// TestPersistOutcomeAllZerosWritesInfoLevel — when every group exits
// 0 the aggregate event lands at info level and the payload's
// `failed` count is zero.
func TestPersistOutcomeAllZerosWritesInfoLevel(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repoID, _ := s.EnsureRepo(ctx, "/r/bar", "bar")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/bar/.wt/a", "a", "main")
	PersistOutcome(ctx, s, repoID, wtID, "precreate", 0, 10,
		RunOutcome{Groups: []GroupOutcome{{Command: "ok", ExitCode: 0}}})

	events, err := s.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{"hook_done"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Level != store.LevelInfo {
		t.Errorf("expected info level, got %s", events[0].Level)
	}
}
