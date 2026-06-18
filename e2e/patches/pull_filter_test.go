//go:build e2e

// Package patches_e2e — pull_filter_test exercises the headline
// invariant of the clean/smudge switch: with EnsureFilter wired,
// `git pull --ff-only` MUST succeed against a worktree carrying
// treeman patches AND the patched key must reappear in the working
// tree after the pull (smudge re-applies during git's checkout
// phase). With the old `--skip-worktree` design this scenario was
// the load-bearing pain point: pull refused with "Your local
// changes to the following files would be overwritten by merge".
package patches_e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/patcher"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/template"
)

// TestPullOverwritesPatchedFile_DotenvIdentitySmudge:
// build a real bare upstream + a downstream clone, install the
// filter (with a `cat`-equivalent identity smudge so the test
// doesn't have to invoke the treeman binary), patch the file
// locally, advance upstream's copy, then `git pull --ff-only` and
// confirm:
//
//  1. pull succeeds (would error with "would be overwritten" under
//     skip-worktree),
//  2. after the pull the working tree carries upstream's new
//     content (smudge re-applies in production; here we verify the
//     prerequisite — git is willing to overwrite at all).
//
// Driving the real treeman binary is out of scope for unit-style
// e2e; the install path is exercised independently by
// TestEnsureFilter_*. This test pins git's behaviour around the
// installed filter config.
func TestPullOverwritesPatchedFile_DotenvIdentitySmudge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp := t.TempDir()

	// Build the upstream and the seeding worktree.
	upstream := filepath.Join(tmp, "upstream.git")
	mustRun(t, tmp, "git", "init", "-q", "--bare", "-b", "master", upstream)
	seed := filepath.Join(tmp, "seed")
	mustRun(t, tmp, "git", "init", "-q", "-b", "master", seed)
	mustRun(t, seed, "git", "config", "user.email", "t@t")
	mustRun(t, seed, "git", "config", "user.name", "t")
	mustRun(t, seed, "git", "remote", "add", "origin", upstream)
	envPath := filepath.Join(seed, ".env.testing")
	if err := os.WriteFile(envPath, []byte("DB_TEST_DATABASE=app_testing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, seed, "git", "add", ".env.testing")
	mustRun(t, seed, "git", "commit", "-q", "-m", "v1")
	mustRun(t, seed, "git", "push", "-q", "origin", "master")

	// Local clone that treeman would manage.
	local := filepath.Join(tmp, "local")
	mustRun(t, tmp, "git", "clone", "-q", "-b", "master", upstream, local)
	mustRun(t, local, "git", "config", "user.email", "t@t")
	mustRun(t, local, "git", "config", "user.name", "t")

	// Patch the local .env.testing as treeman would on wt create.
	tplCtx := template.FromSlug(slug.Slug{Value: "feat-x", Source: slug.SourceTicket})
	patch := config.Patch{
		File: ".env.testing",
		Set:  map[string]string{"DB_TEST_DATABASE": "app_testing_{slug}"},
	}
	if _, err := patcher.Apply(patch, local, tplCtx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Wire the filter for this worktree. We don't drive
	// `patcher.EnsureFilter` here because it points the filter
	// program at `treeman patch-filter` (not on the test PATH).
	// What we ARE validating is the git-side contract: with the
	// info/attributes + filter.* config in place, git accepts a
	// `pull --ff-only` against a working tree whose disk content
	// differs from HEAD. Identity smudge (`cat`) + a clean filter
	// that yields HEAD's content (`git show HEAD:...`) is the
	// minimal wiring that satisfies clean(working) == HEAD.
	mustRun(t, local, "git", "config", "--local", "filter.treeman-patch.clean", "git show HEAD:.env.testing")
	mustRun(t, local, "git", "config", "--local", "filter.treeman-patch.smudge", "cat")
	mustRun(t, local, "git", "config", "--local", "filter.treeman-patch.required", "true")
	if err := os.MkdirAll(filepath.Join(local, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(local, ".git", "info", "attributes"),
		[]byte(".env.testing filter=treeman-patch\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	// Renormalize: pushes the cleaned content into the index so
	// git's pull/merge safety check (which compares stat-cache +
	// index content to working tree) sees the file as matching
	// HEAD. EnsureFilter does the equivalent in production.
	mustRun(t, local, "git", "add", "--renormalize", ".env.testing")

	// Sanity: working file is patched. (`git status` may still show
	// "M" against the stat-cache until git refreshes it; that's a
	// UX detail, not the invariant under test. `git diff --quiet`
	// exercises the filter-aware compare — exit 0 there proves
	// clean(working) == HEAD, which is the precondition for the
	// pull-overwrite contract.)
	body, _ := os.ReadFile(filepath.Join(local, ".env.testing"))
	if !strings.Contains(string(body), "app_testing_feat-x") {
		t.Fatalf("local patch did not write per-worktree value: %q", body)
	}
	if err := exec.Command("git", "-C", local, "diff", "--quiet", ".env.testing").Run(); err != nil {
		t.Fatalf("filter-aware diff must show file clean; got: %v", err)
	}

	// Upstream advances.
	if err := os.WriteFile(envPath, []byte("DB_TEST_DATABASE=app_testing_v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, seed, "git", "commit", "-q", "-am", "v2")
	mustRun(t, seed, "git", "push", "-q", "origin", "master")

	// THIS is the headline assertion: pull must NOT refuse the
	// merge. Pre-filter this exact scenario errored with "Your
	// local changes would be overwritten by merge".
	mustRun(t, local, "git", "pull", "-q", "--ff-only", "origin", "master")

	// After pull, the working file holds upstream's new content
	// (the smudge here is `cat` so we just see upstream verbatim —
	// in production smudge would re-overlay the per-worktree
	// value).
	after, _ := os.ReadFile(filepath.Join(local, ".env.testing"))
	if !strings.Contains(string(after), "app_testing_v2") {
		t.Fatalf("pull did not overwrite the patched file: %q", after)
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
