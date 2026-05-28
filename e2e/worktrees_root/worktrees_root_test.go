//go:build e2e

// Package worktreesroot_e2e covers a non-default `worktrees.root`: every
// other e2e leaves it at the ".worktrees" default. Drives the real
// wt.Create (git worktree add) and asserts the worktree lands under the
// overridden root. No docker: no databases declared, so Create skips the
// engine prepare tail (CreatedNoFinalize).
package worktreesroot_e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/wt"
)

func TestWorktreesRootOverride(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Isolate the store + global config so Create doesn't touch the
	// developer's real DB (it resolves store.DefaultDBPath()).
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "tm.db"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repo := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "main", repo)
	mustGit(t, repo, "config", "user.email", "t@t")
	mustGit(t, repo, "config", "user.name", "t")
	writeFile(t, repo, "README", "hi")
	// Non-default root, no databases (keeps it docker-free).
	writeFile(t, repo, ".treeman.yaml", "worktrees:\n  root: custom-wts\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-q", "-m", "init")

	res, err := wt.Create(context.Background(), wt.CreateRequest{
		RepoRoot: repo,
		Branch:   "feature/x",
		NoFetch:  true,
	}, nil)
	if err != nil {
		t.Fatalf("wt.Create: %v", err)
	}

	want := filepath.Join(repo, "custom-wts", "feature/x")
	if res.WtPath != want {
		t.Errorf("worktree path = %q, want %q (worktrees.root override ignored)", res.WtPath, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("worktree dir not created under custom root: %v", err)
	}
	// And NOT under the default .worktrees.
	if _, err := os.Stat(filepath.Join(repo, ".worktrees")); err == nil {
		t.Errorf("worktree was also created under the default .worktrees — override not honored")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
