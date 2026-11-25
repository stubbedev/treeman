package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// A deleted worktree whose teardown failed is retried on every sweep
// until a teardown succeeds (#171).
func TestRetryPendingTeardowns(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s, config.DefaultLifecycleSlots)
	repoID, err := s.EnsureRepo(ctx, "/r", "r")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := s.EnsureWorktree(ctx, repoID, "/r/.worktrees/a", "a", "feature/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeardownError(ctx, wtID, "Error 1040: Too many connections"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWorktreeDeleted(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	retries := func() []store.Event {
		t.Helper()
		ev, err := s.QueryEvents(ctx, store.EventFilter{
			WorktreeID: wtID, EventTypes: []string{store.EvtDBTeardownRetry}, OldestFirst: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}

	failing := &config.Config{Databases: []config.DatabaseConfig{{Engine: "sqlite", NameTemplate: ""}}}
	retryPendingTeardowns(ctx, st, failing, repoID)
	if pending, err := s.ListPendingTeardowns(ctx, repoID); err != nil || len(pending) != 1 {
		t.Fatalf("after a failed retry: pending = %+v err=%v, want it still queued", pending, err)
	}
	if ev := retries(); len(ev) != 1 || ev[0].Level != store.LevelWarn {
		t.Fatalf("retry events = %+v, want one warning", ev)
	}

	ok := &config.Config{Databases: []config.DatabaseConfig{{Engine: "postgress", NameTemplate: "app_{slug}"}}}
	retryPendingTeardowns(ctx, st, ok, repoID)
	if pending, err := s.ListPendingTeardowns(ctx, repoID); err != nil || len(pending) != 0 {
		t.Fatalf("after a successful retry: pending = %+v err=%v, want none", pending, err)
	}
	if ev := retries(); len(ev) != 2 || ev[1].Level != store.LevelInfo {
		t.Fatalf("retry events = %+v, want a warning then an info", ev)
	}
}
