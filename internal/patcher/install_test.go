package patcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureFilter_LinkedWorktreeAttributesVisible pins the bug that
// shipped with the clean/smudge switch: EnsureFilter used to write
// `info/attributes` under the per-linked-worktree GIT_DIR
// (`.git/worktrees/<name>/info/attributes`), but git only reads
// `info/attributes` from the shared common dir. Result: patched
// files in linked worktrees showed as modified in `git status`
// because git never saw the `filter=treeman-patch` attribute.
//
// The fix writes to `<gitCommonDir>/info/attributes`. This test
// builds a real linked worktree, runs EnsureFilter, and asserts that
// `git check-attr` reports the filter attribute inside the linked
// worktree (the regression site).
func TestEnsureFilter_LinkedWorktreeAttributesVisible(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()

	main := filepath.Join(tmp, "main")
	mustRun(t, tmp, "git", "init", "-q", "-b", "master", main)
	mustRun(t, main, "git", "config", "user.email", "t@t")
	mustRun(t, main, "git", "config", "user.name", "t")
	envPath := filepath.Join(main, ".env.testing")
	if err := os.WriteFile(envPath, []byte("DB=app_testing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, main, "git", "add", ".env.testing")
	mustRun(t, main, "git", "commit", "-q", "-m", "v1")
	mustRun(t, main, "git", "branch", "feature")

	linked := filepath.Join(tmp, "linked")
	mustRun(t, main, "git", "worktree", "add", "-q", linked, "feature")

	ctx := context.Background()
	if err := EnsureFilter(ctx, linked, []string{".env.testing"}); err != nil {
		t.Fatalf("EnsureFilter: %v", err)
	}

	// The attributes file must land in the common dir, not the
	// per-linked-worktree GIT_DIR. Both checks pin the regression.
	commonAttrs := filepath.Join(main, ".git", "info", "attributes")
	if body, err := os.ReadFile(commonAttrs); err != nil {
		t.Fatalf("common-dir info/attributes missing: %v", err)
	} else if !strings.Contains(string(body), ".env.testing filter="+FilterName) {
		t.Fatalf("common-dir info/attributes missing filter line:\n%s", body)
	}
	perWtAttrs := filepath.Join(main, ".git", "worktrees", "linked", "info", "attributes")
	if _, err := os.Stat(perWtAttrs); err == nil {
		t.Fatalf("per-worktree info/attributes must not be written (git ignores it): %s", perWtAttrs)
	}

	// The headline assertion: `git check-attr` inside the linked
	// worktree reports the filter attribute. Pre-fix this returned
	// "unspecified" because git couldn't see the per-worktree file.
	out := runCapture(t, linked, "git", "check-attr", "filter", ".env.testing")
	if !strings.Contains(out, "filter: "+FilterName) {
		t.Fatalf("linked worktree must see filter attribute, got: %q", out)
	}

	// And the main worktree sees it too (common-dir bleed is by
	// design; filter falls through to pass-through when the cwd's
	// config doesn't patch the file). Pin that as well so the next
	// person who tries to "fix" the bleed knows it's load-bearing.
	out = runCapture(t, main, "git", "check-attr", "filter", ".env.testing")
	if !strings.Contains(out, "filter: "+FilterName) {
		t.Fatalf("main worktree also sees filter attribute (shared common dir), got: %q", out)
	}
}

// TestEnsureFilter_ReplacesTreemanBlock guards block replacement so
// repeated finalize runs don't leak stale filter lines for files no
// longer in `patches[]`.
func TestEnsureFilter_ReplacesTreemanBlock(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	mustRun(t, tmp, "git", "init", "-q", "-b", "master", repo)
	mustRun(t, repo, "git", "config", "user.email", "t@t")
	mustRun(t, repo, "git", "config", "user.name", "t")
	for _, f := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-q", "-m", "v1")

	// Pre-seed an unrelated user attribute that EnsureFilter must
	// preserve.
	attrPath := filepath.Join(repo, ".git", "info", "attributes")
	if err := os.MkdirAll(filepath.Dir(attrPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attrPath, []byte("*.txt diff=foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := EnsureFilter(ctx, repo, []string{"a", "b"}); err != nil {
		t.Fatalf("EnsureFilter first: %v", err)
	}
	if err := EnsureFilter(ctx, repo, []string{"c"}); err != nil {
		t.Fatalf("EnsureFilter second: %v", err)
	}

	body, err := os.ReadFile(attrPath)
	if err != nil {
		t.Fatalf("read attrs: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "*.txt diff=foo") {
		t.Errorf("user attribute clobbered:\n%s", got)
	}
	if !strings.Contains(got, "c filter="+FilterName) {
		t.Errorf("second run's filter line missing:\n%s", got)
	}
	if strings.Contains(got, "a filter=") || strings.Contains(got, "b filter=") {
		t.Errorf("stale filter lines from first run not removed:\n%s", got)
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
