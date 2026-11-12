package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSyncTargetOutsideARepoFailsInsteadOfSyncingEveryRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	t.Chdir(dir)
	got, err := resolveSyncTarget("", false)
	if err == nil {
		t.Fatalf("resolveSyncTarget outside a repo = %q, nil; want an error instead of the every-repo scope", got)
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Errorf("error must point at --all for an every-repo sync: %v", err)
	}
}

func TestResolveSyncTargetFailsWhenTheWorkingDirectoryIsGone(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveSyncTarget("", false); err == nil {
		t.Fatalf("resolveSyncTarget with a deleted cwd = %q, nil; want an error", got)
	}
}

func TestResolveSyncTargetScopesToTheCurrentRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveSyncTarget("", false)
	if err != nil {
		t.Fatalf("resolveSyncTarget inside a repo: %v", err)
	}
	if got != want {
		t.Errorf("resolveSyncTarget inside %s = %q, want the repo root %q", sub, got, want)
	}
}

func TestResolveSyncTargetExplicitScopes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	t.Chdir(dir)

	got, err := resolveSyncTarget("", true)
	if err != nil || got != "" {
		t.Errorf("resolveSyncTarget(--all) = %q, %v; want the empty every-repo scope", got, err)
	}

	got, err = resolveSyncTarget("some/repo", false)
	if err != nil {
		t.Fatalf("resolveSyncTarget(path): %v", err)
	}
	if want := filepath.Join(dir, "some", "repo"); got != want {
		t.Errorf("resolveSyncTarget(path) = %q, want %q", got, want)
	}

	if got, err := resolveSyncTarget("some/repo", true); err == nil {
		t.Errorf("resolveSyncTarget(path, --all) = %q, nil; want a mutual-exclusion error", got)
	}
}
