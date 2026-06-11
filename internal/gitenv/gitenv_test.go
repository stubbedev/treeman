package gitenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initRepo creates a real git repo with one commit and returns its
// path. Linked-worktree resolution can't be faked with fixtures — the
// gitlink + git-common-dir behavior under test IS git's.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-b", "main")
	run(t, dir, "config", "user.email", "t@t")
	run(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-m", "init")
	// Canonicalise: macOS TempDir is a /var → /private/var symlink and
	// MainRoot EvalSymlinks-es its result.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestMainRootFromMainCheckoutAndSubdir(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, start := range []string{repo, sub} {
		got, err := MainRoot(context.Background(), start)
		if err != nil || got != repo {
			t.Errorf("MainRoot(%s) = %q, %v; want %q", start, got, err, repo)
		}
	}
}

func TestMainRootFromLinkedWorktree(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(repo, ".worktrees", "feat")
	run(t, repo, "worktree", "add", "-b", "feat", wt)

	got, err := MainRoot(context.Background(), wt)
	if err != nil || got != repo {
		t.Fatalf("MainRoot(linked wt) = %q, %v; want %q", got, err, repo)
	}
	// And from a subdir inside the linked worktree.
	sub := filepath.Join(wt, "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = MainRoot(context.Background(), sub)
	if err != nil || got != repo {
		t.Fatalf("MainRoot(linked wt subdir) = %q, %v; want %q", got, err, repo)
	}
}

func TestMainRootTreemanYamlFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".treeman.yaml"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := MainRoot(context.Background(), sub)
	if err != nil {
		t.Fatalf("MainRoot: %v", err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != want {
		t.Errorf("MainRoot = %q, want %q", gotResolved, want)
	}
}

func TestIsLinkedWorktree(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(repo, ".worktrees", "feat")
	run(t, repo, "worktree", "add", "-b", "feat", wt)
	if IsLinkedWorktree(repo) {
		t.Error("main checkout flagged as linked worktree")
	}
	if !IsLinkedWorktree(wt) {
		t.Error("linked worktree not detected")
	}
	if IsLinkedWorktree(t.TempDir()) {
		t.Error("non-repo dir flagged as linked worktree")
	}
}

func TestIsWorktreeClean(t *testing.T) {
	repo := initRepo(t)
	clean, err := IsWorktreeClean(context.Background(), repo)
	if err != nil || !clean {
		t.Fatalf("fresh repo: clean=%v err=%v, want true", clean, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean, err = IsWorktreeClean(context.Background(), repo)
	if err != nil || clean {
		t.Fatalf("untracked file: clean=%v err=%v, want false", clean, err)
	}
}

func TestHasUnpushedCommitsNoUpstream(t *testing.T) {
	repo := initRepo(t)
	// No tracking branch → "nothing can be unpushed", not an error.
	unpushed, err := HasUnpushedCommits(context.Background(), repo)
	if err != nil || unpushed {
		t.Fatalf("no upstream: unpushed=%v err=%v, want false, nil", unpushed, err)
	}
}

func TestDetectBranch(t *testing.T) {
	repo := initRepo(t)
	if got := DetectBranch(context.Background(), repo); got != "main" {
		t.Errorf("main checkout branch = %q, want main", got)
	}
	wt := filepath.Join(repo, ".worktrees", "feat")
	run(t, repo, "worktree", "add", "-b", "feat", wt)
	if got := DetectBranch(context.Background(), wt); got != "feat" {
		t.Errorf("linked worktree branch = %q, want feat (gitlink follow)", got)
	}
	// Detached HEAD → "".
	run(t, repo, "checkout", "--detach")
	if got := DetectBranch(context.Background(), repo); got != "" {
		t.Errorf("detached HEAD branch = %q, want empty", got)
	}
}
