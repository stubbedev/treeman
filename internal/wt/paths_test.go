package wt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

func TestWorktreesRoot(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		got := WorktreesRoot(config.Config{}, "/repo")
		want := filepath.FromSlash("/repo/.worktrees")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("relative override", func(t *testing.T) {
		cfg := config.Config{Worktrees: config.WorktreesConfig{Root: "wt"}}
		got := WorktreesRoot(cfg, "/repo")
		want := filepath.FromSlash("/repo/wt")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("absolute override", func(t *testing.T) {
		cfg := config.Config{Worktrees: config.WorktreesConfig{Root: "/abs/wt"}}
		got := WorktreesRoot(cfg, "/repo")
		want := "/abs/wt"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// gitRepo initializes a temp git repo with one commit on `branch`.
// Returns the absolute repo path.
func gitRepo(t *testing.T, branch string) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", branch)
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-m", "initial")
	return repo
}

func TestDetectDefaultBranchFallsBackToHEAD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := gitRepo(t, "develop")
	got := DetectDefaultBranch(context.Background(), repo)
	if got != "develop" {
		t.Errorf("got %q, want develop", got)
	}
}

func TestRefExists(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := gitRepo(t, "main")
	ctx := context.Background()
	if !RefExistsLocal(ctx, repo, "main") {
		t.Error("RefExistsLocal(main) = false, want true")
	}
	if RefExistsLocal(ctx, repo, "does-not-exist") {
		t.Error("RefExistsLocal(does-not-exist) = true, want false")
	}
	if RefExistsRemote(ctx, repo, "main") {
		t.Error("RefExistsRemote(main) = true (no remote configured), want false")
	}
}

func TestPruneEmptyParents(t *testing.T) {
	// PruneEmptyParents is called AFTER `git worktree remove` has
	// already deleted `start` — it only cleans up the now-empty
	// parents leading up to the worktrees root.
	root := t.TempDir()
	wtRoot := filepath.Join(root, "wts")
	if err := os.MkdirAll(filepath.Join(wtRoot, "feature"), 0o755); err != nil {
		t.Fatal(err)
	}
	deletedLeaf := filepath.Join(wtRoot, "feature", "branch") // never created
	PruneEmptyParents(deletedLeaf, wtRoot)
	// `feature` was empty so it should be pruned; the worktrees
	// root itself stays.
	if pathExists(filepath.Join(wtRoot, "feature")) {
		t.Error("empty parent 'feature' should have been pruned")
	}
	if !pathExists(wtRoot) {
		t.Error("worktrees root should not have been pruned")
	}
}

func TestPruneEmptyParentsStopsOnNonEmpty(t *testing.T) {
	root := t.TempDir()
	wtRoot := filepath.Join(root, "wts")
	// Sibling keeps `feature` non-empty.
	sibling := filepath.Join(wtRoot, "feature", "other")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	deletedLeaf := filepath.Join(wtRoot, "feature", "branch") // never created
	PruneEmptyParents(deletedLeaf, wtRoot)
	if !pathExists(sibling) {
		t.Error("sibling should still exist")
	}
	if !pathExists(filepath.Join(wtRoot, "feature")) {
		t.Error("non-empty 'feature' parent must be preserved")
	}
}

func TestNoopSinkDiscards(t *testing.T) {
	// Just exercise — should not panic.
	var s Sink = NoopSink{}
	s.OK("x %d", 1)
	s.Warn("x %d", 1)
	s.Info("x %d", 1)
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
