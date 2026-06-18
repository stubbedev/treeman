package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

// TestDeriveStatusBucketPrepareClearsError asserts a successful
// prepare:end newer than a prior worktree:create:error flips the
// worktree back to stable. A standalone prepare_run (manual or via
// repair) emits no worktree:create:end, so without folding prepare
// terminal events into the derivation the stale error pinned the
// worktree to "failed" forever. Regression for that.
func TestDeriveStatusBucketPrepareClearsError(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r", "r")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/.wt/feat", "feat", "feat")

	write := func(level, evt string) {
		if err := s.WriteEvent(ctx, level, evt, "m", repoID, wtID, "", 0, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Failed prepare, then a later successful one.
	write(store.LevelError, store.EvtWorktreeCreateError)
	if _, b := deriveStatusBucket(ctx, s, wtID); b != "failed" {
		t.Fatalf("after create:error bucket=%q, want failed", b)
	}
	write(store.LevelInfo, store.EvtPrepareEnd)
	if _, b := deriveStatusBucket(ctx, s, wtID); b != "stable" {
		t.Fatalf("after recovery prepare:end bucket=%q, want stable", b)
	}

	// A later prepare:error must drag it back to failed.
	write(store.LevelError, store.EvtPrepareError)
	if _, b := deriveStatusBucket(ctx, s, wtID); b != "failed" {
		t.Fatalf("after prepare:error bucket=%q, want failed", b)
	}
}
