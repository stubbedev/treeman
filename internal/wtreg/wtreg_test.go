package wtreg

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

// requireGit skips the test when git isn't on PATH (e.g. minimal CI).
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func TestRepairRegistersMissingWorktrees(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// Need an initial commit before `git worktree add` will work.
	for _, args := range [][]string{
		{"-C", repo, "config", "user.email", "t@t"},
		{"-C", repo, "config", "user.name", "t"},
		{"-C", repo, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	wt := filepath.Join(tmp, "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", wt, "-b", "feature").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v: %s", err, out)
	}
	// macOS resolves /var → /private/var when git writes the gitdir
	// pointer, so the path our parser reports back is the realpath
	// form. Resolve `wt` the same way to compare apples-to-apples.
	wtResolved, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(tmp, "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	res, err := Repair(ctx, st, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Registered) != 1 || res.Registered[0] != wtResolved {
		t.Errorf("Registered = %+v, want [%s]", res.Registered, wtResolved)
	}

	// Second call should be a no-op.
	res2, err := Repair(ctx, st, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Registered) != 0 || len(res2.Unregistered) != 0 {
		t.Errorf("second Repair not idempotent: %+v", res2)
	}
}

func TestRepairPreservesMainWorktreeRow(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	st, err := store.Open(ctx, filepath.Join(tmp, "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	repoID, _ := st.EnsureRepo(ctx, repo, "repo")
	// Simulate `treeman main enable` having enrolled the repo root as
	// an active is_main=1 worktree row.
	mainID, err := st.EnsureMainWorktree(ctx, repoID, repo, "main_slug", "main")
	if err != nil {
		t.Fatal(err)
	}
	// And a genuinely-dead linked-wt row that should still get
	// unregistered.
	_, _ = st.EnsureWorktree(ctx, repoID, "/nonexistent-wt", "dead", "feature")

	res, err := Repair(ctx, st, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Unregistered {
		if p == repo {
			t.Errorf("Repair unregistered the active main worktree row at %s", repo)
		}
	}
	if len(res.Unregistered) != 1 || res.Unregistered[0] != "/nonexistent-wt" {
		t.Errorf("Unregistered = %+v, want [/nonexistent-wt]", res.Unregistered)
	}

	// Verify the main row is still active.
	row, err := st.LookupMainWorktree(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != mainID {
		t.Errorf("main row missing after Repair: got id=%d want %d", row.ID, mainID)
	}
}

func TestRepairUnregistersMissingFromGit(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	st, err := store.Open(ctx, filepath.Join(tmp, "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	repoID, _ := st.EnsureRepo(ctx, repo, "repo")
	_, _ = st.EnsureWorktree(ctx, repoID, "/nonexistent-wt", "slug", "branch")

	res, err := Repair(ctx, st, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unregistered) != 1 || res.Unregistered[0] != "/nonexistent-wt" {
		t.Errorf("Unregistered = %+v, want [/nonexistent-wt]", res.Unregistered)
	}
}
