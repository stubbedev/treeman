package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
)

// TestEnrollMainWorktreeFastPathSkipsRepoWithoutConfig covers the
// boot hot path: a registered repo that hasn't grown a .treeman.yaml
// yet should produce a zero row + nil error without parsing config.
// Catches regressions of the os.Stat fast path.
func TestEnrollMainWorktreeFastPathSkipsRepoWithoutConfig(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	gitRun(t, "", "init", "-q", "-b", "main", repo)
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "--allow-empty", "-q", "-m", "init")
	// Deliberately NO .treeman.yaml.

	s, err := store.Open(ctx, filepath.Join(tmp, "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)

	id, err := EnrollMainWorktree(ctx, st, repo)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if id != 0 {
		t.Errorf("expected zero row id when no config exists, got %d", id)
	}
}

// TestEnrollMainWorktreeEnableDisableRoundTrip drives EnrollMainWorktree
// through enable → disable → re-enable, asserting the worktrees table
// follows the config and the slug is branch-aware.
func TestEnrollMainWorktreeEnableDisableRoundTrip(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()

	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	gitRun(t, "", "init", "-q", "-b", "develop", repo)
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "--allow-empty", "-q", "-m", "init")

	// .treeman.yaml opting in.
	cfgPath := filepath.Join(repo, ".treeman.yaml")
	if err := os.WriteFile(cfgPath, []byte("main_worktree:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(ctx, filepath.Join(tmp, "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)

	// First enroll — fresh row.
	id, err := EnrollMainWorktree(ctx, st, repo)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero row id")
	}
	repoID, _ := s.EnsureRepo(ctx, repo, "repo")
	row, _ := s.LookupMainWorktree(ctx, repoID)
	if !row.IsMain || row.Branch != "develop" || row.Slug != "main_develop" {
		t.Errorf("enrolled row mismatch: %+v", row)
	}

	// Switch branch + re-enroll — slug must track HEAD.
	gitRun(t, repo, "checkout", "-q", "-b", "feature/foo")
	if _, err := EnrollMainWorktree(ctx, st, repo); err != nil {
		t.Fatal(err)
	}
	row, _ = s.LookupMainWorktree(ctx, repoID)
	if row.Slug != "main_feature_foo" {
		t.Errorf("post-checkout slug = %q, want main_feature_foo", row.Slug)
	}

	// Disable — config flips, re-enroll soft-deletes the row.
	if err := os.WriteFile(cfgPath, []byte("main_worktree:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolve.InvalidateConfigCache()
	zero, err := EnrollMainWorktree(ctx, st, repo)
	if err != nil {
		t.Fatal(err)
	}
	if zero != 0 {
		t.Errorf("disable should return 0, got %d", zero)
	}
	row, _ = s.LookupMainWorktree(ctx, repoID)
	if row.ID != 0 {
		t.Errorf("active main row should be empty post-disable, got %+v", row)
	}

	// Re-enable — must resurrect the same row id with refreshed slug.
	if err := os.WriteFile(cfgPath, []byte("main_worktree:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolve.InvalidateConfigCache()
	id3, err := EnrollMainWorktree(ctx, st, repo)
	if err != nil {
		t.Fatal(err)
	}
	if id3 != id {
		t.Errorf("re-enable should reuse row id %d, got %d", id, id3)
	}
}
