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
	mustRun(t, tmp, "init", "-q", "-b", "master", main)
	mustRun(t, main, "config", "user.email", "t@t")
	mustRun(t, main, "config", "user.name", "t")
	envPath := filepath.Join(main, ".env.testing")
	if err := os.WriteFile(envPath, []byte("DB=app_testing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, main, "add", ".env.testing")
	mustRun(t, main, "commit", "-q", "-m", "v1")
	mustRun(t, main, "branch", "feature")

	linked := filepath.Join(tmp, "linked")
	mustRun(t, main, "worktree", "add", "-q", linked, "feature")

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

// TestEnsureFilter_StripsLegacyPerWorktreeAttributes:
// treeman <= 2.4.1 wrote `info/attributes` under the per-linked-
// worktree GIT_DIR. Git silently ignored it. On upgrade, EnsureFilter
// must move the wiring to common-dir AND strip the orphan so users
// don't see two copies drifting. Unrelated user lines in the per-wt
// file (unlikely but possible) survive.
func TestEnsureFilter_StripsLegacyPerWorktreeAttributes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main")
	mustRun(t, tmp, "init", "-q", "-b", "master", main)
	mustRun(t, main, "config", "user.email", "t@t")
	mustRun(t, main, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(main, ".env.testing"), []byte("DB=v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, main, "add", ".env.testing")
	mustRun(t, main, "commit", "-q", "-m", "v1")
	mustRun(t, main, "branch", "feature")
	linked := filepath.Join(tmp, "linked")
	mustRun(t, main, "worktree", "add", "-q", linked, "feature")

	// Seed a legacy per-worktree attributes file as treeman 2.4.1
	// would have produced it. Drop a real user line in there too so
	// we can prove migration is surgical.
	perWtAttrs := filepath.Join(main, ".git", "worktrees", "linked", "info", "attributes")
	if err := os.MkdirAll(filepath.Dir(perWtAttrs), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "*.log diff=foo\n" + attrsHeader + "\n.env.testing filter=" + FilterName + "\n"
	if err := os.WriteFile(perWtAttrs, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureFilter(context.Background(), linked, []string{".env.testing"}); err != nil {
		t.Fatalf("EnsureFilter: %v", err)
	}

	// User line survived; treeman block stripped.
	body, err := os.ReadFile(perWtAttrs)
	if err != nil {
		t.Fatalf("per-wt attrs gone but it had user content: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "*.log diff=foo") {
		t.Errorf("user line clobbered:\n%s", got)
	}
	if strings.Contains(got, attrsHeader) || strings.Contains(got, "filter="+FilterName) {
		t.Errorf("treeman block not stripped from per-wt file:\n%s", got)
	}

	// Common-dir attrs has the live wiring.
	out := runCapture(t, linked, "git", "check-attr", "filter", ".env.testing")
	if !strings.Contains(out, "filter: "+FilterName) {
		t.Fatalf("filter not wired in common dir, got: %q", out)
	}
}

// TestEnsureFilter_DeletesEmptyLegacyPerWorktreeAttributes: when the
// legacy per-wt file ONLY contained the treeman block (the common
// case), strip+empty → delete the file outright so no orphan remains.
func TestEnsureFilter_DeletesEmptyLegacyPerWorktreeAttributes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main")
	mustRun(t, tmp, "init", "-q", "-b", "master", main)
	mustRun(t, main, "config", "user.email", "t@t")
	mustRun(t, main, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(main, ".env.testing"), []byte("DB=v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, main, "add", ".env.testing")
	mustRun(t, main, "commit", "-q", "-m", "v1")
	mustRun(t, main, "branch", "feature")
	linked := filepath.Join(tmp, "linked")
	mustRun(t, main, "worktree", "add", "-q", linked, "feature")
	perWtAttrs := filepath.Join(main, ".git", "worktrees", "linked", "info", "attributes")
	if err := os.MkdirAll(filepath.Dir(perWtAttrs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(perWtAttrs, []byte(attrsHeader+"\n.env.testing filter="+FilterName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureFilter(context.Background(), linked, []string{".env.testing"}); err != nil {
		t.Fatalf("EnsureFilter: %v", err)
	}
	if _, err := os.Stat(perWtAttrs); err == nil {
		t.Fatalf("legacy per-wt attrs file should be deleted: %s", perWtAttrs)
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
	mustRun(t, tmp, "init", "-q", "-b", "master", repo)
	mustRun(t, repo, "config", "user.email", "t@t")
	mustRun(t, repo, "config", "user.name", "t")
	for _, f := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, repo, "add", ".")
	mustRun(t, repo, "commit", "-q", "-m", "v1")

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

func mustRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
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
