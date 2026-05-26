package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureMainWorktreeInsertsAndResurrects covers the two paths the
// daemon's enroll flow uses:
//  1. First enable → fresh INSERT with is_main=1.
//  2. Disable (soft delete) → re-enable → resurrect same row with
//     is_main=1 restored.
func TestEnsureMainWorktreeInsertsAndResurrects(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	repoID, _ := s.EnsureRepo(ctx, "/repos/x", "x")
	id1, err := s.EnsureMainWorktree(ctx, repoID, "/repos/x", "main_develop", "develop")
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.LookupMainWorktree(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != id1 || !row.IsMain {
		t.Fatalf("first ensure: row=%+v", row)
	}

	if err := s.MarkWorktreeDeleted(ctx, id1); err != nil {
		t.Fatal(err)
	}
	dead, _ := s.LookupMainWorktree(ctx, repoID)
	if dead.ID != 0 {
		t.Errorf("soft-deleted row should not appear active: %+v", dead)
	}

	id2, err := s.EnsureMainWorktree(ctx, repoID, "/repos/x", "main_feature", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 {
		t.Errorf("resurrect should reuse row id: got %d want %d", id2, id1)
	}
	row, _ = s.LookupMainWorktree(ctx, repoID)
	if !row.IsMain || row.Slug != "main_feature" || row.Branch != "feature" {
		t.Errorf("resurrected row not restored correctly: %+v", row)
	}
}

// TestEnsureMainWorktreeUniquePerRepo confirms the partial unique
// index refuses a second active main row for the same repo. Inserting
// a *separate* path with is_main=1 should fail — the schema treats
// "one main per repo" as an invariant.
func TestEnsureMainWorktreeUniquePerRepo(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	repoID, _ := s.EnsureRepo(ctx, "/repos/x", "x")
	if _, err := s.EnsureMainWorktree(ctx, repoID, "/repos/x", "main_a", "a"); err != nil {
		t.Fatal(err)
	}
	// Direct INSERT bypasses the EnsureMainWorktree path-based dedup
	// and exercises the partial unique index directly.
	_, err = s.DB.ExecContext(ctx,
		"INSERT INTO worktrees(repo_id, path, slug, branch, created_at, is_main) VALUES (?, ?, ?, ?, ?, 1)",
		repoID, "/repos/x/other", "main_b", "b", 1)
	if err == nil {
		t.Fatalf("expected unique index violation on second active is_main row")
	}
	if !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("want UNIQUE constraint error, got: %v", err)
	}
}
