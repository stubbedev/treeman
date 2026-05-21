package patcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkipWorktreeAppliesToLinkedWorktreeIndex is the regression
// for the v2.2.0 bug: SkipWorktree was invoked with the MAIN repo
// path, but a linked worktree has its own index — so the bit was
// set on the wrong index and `git ls-files -v` in the worktree
// showed `H` instead of `S`.
//
// Setup: a tracked `.env.testing` in `main` + a linked worktree at
// `main/.worktrees/wt1`. Run SkipWorktree against the worktree
// dir. Assert `git ls-files -v` inside the worktree reports `S`.
func TestSkipWorktreeAppliesToLinkedWorktreeIndex(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main")
	mustRun(t, tmp, "git", "init", "-q", "-b", "master", main)
	mustRun(t, main, "git", "config", "user.email", "t@example")
	mustRun(t, main, "git", "config", "user.name", "tester")

	envPath := filepath.Join(main, ".env.testing")
	if err := os.WriteFile(envPath, []byte("DB_PASSWORD=base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, main, "git", "add", ".env.testing")
	mustRun(t, main, "git", "commit", "-q", "-m", "init")

	// `master` is already checked out in `main`; create a new
	// branch for the linked worktree.
	wt := filepath.Join(main, ".worktrees", "wt1")
	mustRun(t, main, "git", "worktree", "add", "-q", "-b", "wt1", wt, "master")

	wtEnv := filepath.Join(wt, ".env.testing")
	if err := os.WriteFile(wtEnv, []byte("DB_PASSWORD=patched\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	applied, err := SkipWorktree(wt, wtEnv)
	if err != nil {
		t.Fatalf("SkipWorktree: %v", err)
	}
	if !applied {
		t.Fatal("SkipWorktree returned false despite the file being tracked")
	}

	out := runCapture(t, wt, "git", "ls-files", "-v", ".env.testing")
	if !strings.HasPrefix(out, "S ") {
		t.Errorf("ls-files -v after SkipWorktree: %q (want leading 'S ')", out)
	}

	// Main repo index must NOT have been touched.
	mainOut := runCapture(t, main, "git", "ls-files", "-v", ".env.testing")
	if strings.HasPrefix(mainOut, "S ") {
		t.Errorf("main index unexpectedly marked skip-worktree: %q", mainOut)
	}
}

// TestSkipWorktreeIdempotent — repeated invocations on an already-
// skipped file must succeed and leave the bit set.
func TestSkipWorktreeIdempotent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	mustRun(t, tmp, "git", "init", "-q", "-b", "master", tmp)
	mustRun(t, tmp, "git", "config", "user.email", "t@example")
	mustRun(t, tmp, "git", "config", "user.name", "tester")
	if err := os.WriteFile(filepath.Join(tmp, "tracked.env"), []byte("X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, tmp, "git", "add", "tracked.env")
	mustRun(t, tmp, "git", "commit", "-q", "-m", "init")
	for i := 0; i < 3; i++ {
		applied, err := SkipWorktree(tmp, filepath.Join(tmp, "tracked.env"))
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if !applied {
			t.Fatalf("iter %d: applied=false", i)
		}
	}
	out := runCapture(t, tmp, "git", "ls-files", "-v", "tracked.env")
	if !strings.HasPrefix(out, "S ") {
		t.Errorf("ls-files -v: %q (want 'S ')", out)
	}
}

// TestSkipWorktreeNoOpOnUntracked covers the .env.testing-is-
// gitignored case: function must return (false, nil) — not an error.
func TestSkipWorktreeNoOpOnUntracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	mustRun(t, tmp, "git", "init", "-q", "-b", "master", tmp)
	mustRun(t, tmp, "git", "config", "user.email", "t@example")
	mustRun(t, tmp, "git", "config", "user.name", "tester")
	if err := os.WriteFile(filepath.Join(tmp, "untracked.env"), []byte("X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	applied, err := SkipWorktree(tmp, filepath.Join(tmp, "untracked.env"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if applied {
		t.Error("applied=true for an untracked file")
	}
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func runCapture(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}
