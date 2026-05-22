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

func TestRemoveRepoCascadesChildren(t *testing.T) {
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
	wtID, err := s.EnsureWorktree(ctx, repoID, "/repos/foo/.worktrees/x", "proj_1", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteEvent(ctx, LevelInfo, "wt_finalize_start", "hi",
		repoID, wtID, "", 0, nil); err != nil {
		t.Fatal(err)
	}

	// Refuses with active worktree.
	n, err := s.CountActiveWorktreesForRepo(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("active count: %d", n)
	}

	if err := s.MarkWorktreeDeleted(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRepo(ctx, repoID); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}

	var repos int
	_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos").Scan(&repos)
	if repos != 0 {
		t.Errorf("repos remaining: %d", repos)
	}
	var wts int
	_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM worktrees").Scan(&wts)
	if wts != 0 {
		t.Errorf("worktrees remaining: %d", wts)
	}
	var evs int
	_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&evs)
	if evs != 0 {
		t.Errorf("events remaining: %d", evs)
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
