package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIsGitWorktreeDir asserts the `wt go` filesystem fallback gate: a
// directory under .worktrees/ only resolves when git itself lists it
// as a linked worktree, never for a stale non-git leftover (#34).
func TestIsGitWorktreeDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")

	// A live linked worktree counts…
	run("worktree", "add", "-b", "feature/live", filepath.Join(repo, ".worktrees", "live"))
	if !isGitWorktreeDir(ctx, repo, filepath.Join(repo, ".worktrees", "live")) {
		t.Error("live linked worktree must resolve")
	}
	// …a leftover non-git dir (e.g. node_modules/.cache after teardown)
	// does not…
	stale := filepath.Join(repo, ".worktrees", "feature/stale")
	if err := os.MkdirAll(filepath.Join(stale, "frontend/node_modules/.cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isGitWorktreeDir(ctx, repo, stale) {
		t.Error("stale non-git dir must not resolve")
	}
	// …and neither does a random path under .worktrees/.
	if isGitWorktreeDir(ctx, repo, filepath.Join(repo, ".worktrees", "never-existed")) {
		t.Error("missing dir must not resolve")
	}
}
