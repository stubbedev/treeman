package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Re-opening should be a no-op.
	s2, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestEnsureRepoAndWorktree(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	repoID, err := s.EnsureRepo(ctx, "/repos/foo", "foo")
	if err != nil {
		t.Fatal(err)
	}
	repoID2, _ := s.EnsureRepo(ctx, "/repos/foo", "foo")
	if repoID != repoID2 {
		t.Errorf("expected idempotent: %d vs %d", repoID, repoID2)
	}

	wtID, err := s.EnsureWorktree(ctx, repoID, "/repos/foo/.worktrees/x", "proj_1", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if wtID == 0 {
		t.Fatal("zero wt id")
	}

	paths, err := s.ListRepoPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/repos/foo" {
		t.Errorf("unexpected: %v", paths)
	}
}

func TestWriteEvent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.WriteEvent(ctx, LevelInfo, "wt_finalize_start", "hi",
		0, 0, "", 0, map[string]string{"engine": "mysql"}); err != nil {
		t.Fatal(err)
	}
	var n int
	row := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM events")
	if err := row.Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("want 1 event, got %d", n)
	}
}
