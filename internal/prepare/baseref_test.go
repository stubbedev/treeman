package prepare

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestStripRemotePrefix(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput()
	if err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// A remote named "origin" must exist for the prefix strip to fire.
	gitRun(t, repo, "remote", "add", "origin", repo)

	cases := []struct{ in, want string }{
		{"origin/develop", "develop"},
		{"origin/release/9.4.0", "release/9.4.0"},
		{"develop", "develop"},
		{"upstream/main", "upstream/main"}, // "upstream" not a configured remote
	}
	for _, c := range cases {
		if got := stripRemotePrefix(context.Background(), repo, c.in); got != c.want {
			t.Errorf("stripRemotePrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderWorktreeBaseDB(t *testing.T) {
	cfg := &config.Config{
		Databases: []config.DatabaseConfig{
			{Engine: "mysql", NameTemplate: "kontainer_{slug}"},
		},
	}
	name, ok, err := renderWorktreeBaseDB(cfg, 0, scopeName, slug.Slug{Value: "kon_1234"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || name != "kontainer_kon_1234" {
		t.Errorf("got (%q, %v)", name, ok)
	}
	// Out-of-range index reports not-found rather than panicking.
	if _, ok, _ := renderWorktreeBaseDB(cfg, 5, scopeName, slug.Slug{Value: "x"}); ok {
		t.Errorf("expected ok=false for out-of-range dbIdx")
	}
}

// TestResolveBaseBranch_FallsBackToMainWhenNoUpstream covers issue
// #7: GitFlow feature branches off `develop` end up with no
// `@{upstream}` (their `branch.<name>.merge` points at themselves on
// the remote, not at develop), so `baseBranchOf` returns empty. The
// main-worktree fallback should kick in: when the repo root sits on
// `develop` and `git merge-base feature/X develop` is non-empty,
// `develop` is the base.
func TestResolveBaseBranch_FallsBackToMainWhenNoUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v1")
	// Feature branch off develop, no upstream wired.
	gitRun(t, repo, "branch", "feature/KON-1")
	gitRun(t, repo, "checkout", "-q", "feature/KON-1")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v2")
	// HEAD back on develop so resolveBaseBranch's main-branch lookup
	// (rev-parse HEAD on repoRoot) returns "develop".
	gitRun(t, repo, "checkout", "-q", "develop")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	got := resolveBaseBranch(ctx, st, repo, 0, "feature/KON-1")
	if got != "develop" {
		t.Fatalf("want develop (main-wt fallback), got %q", got)
	}
}

// TestResolveBaseBranch_PrefersEnrolledMainWorktreeRow exercises the
// `main_worktree.enabled: true` path — the store row wins over
// reading HEAD off the repo root checkout, so users who enroll the
// main wt onto a different branch than what's checked out at the
// repo root still get the right fallback.
func TestResolveBaseBranch_PrefersEnrolledMainWorktreeRow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "master")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v1")
	gitRun(t, repo, "branch", "develop")
	gitRun(t, repo, "branch", "feature/KON-1")
	gitRun(t, repo, "checkout", "-q", "feature/KON-1")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v2")
	// Repo root left on master to prove the enrolled row wins.
	gitRun(t, repo, "checkout", "-q", "master")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repo, "test-repo")
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	if _, err := st.EnsureMainWorktree(ctx, repoID, repo, "main_develop", "develop"); err != nil {
		t.Fatalf("EnsureMainWorktree: %v", err)
	}

	got := resolveBaseBranch(ctx, st, repo, repoID, "feature/KON-1")
	if got != "develop" {
		t.Fatalf("want develop (enrolled row), got %q", got)
	}
}

// TestResolveBaseBranch_EmptyWhenNoSharedHistory protects against
// seeding from an unrelated DB: a brand-new orphan branch with no
// common ancestor with the main wt branch must not silently inherit
// data, even when the main-wt fallback would otherwise return a
// branch name.
func TestResolveBaseBranch_EmptyWhenNoSharedHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v1")
	gitRun(t, repo, "checkout", "-q", "--orphan", "orphan")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "orphan-v1")
	gitRun(t, repo, "checkout", "-q", "develop")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	got := resolveBaseBranch(ctx, st, repo, 0, "orphan")
	if got != "" {
		t.Fatalf("want empty (no common ancestor), got %q", got)
	}
}

// TestResolveBaseBranch_EmptyWhenBranchEqualsMain stops the fallback
// from looping the main branch back onto itself. Caller treats "" as
// "no parent available" and falls through to the dump path.
func TestResolveBaseBranch_EmptyWhenBranchEqualsMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v1")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if got := resolveBaseBranch(ctx, st, repo, 0, "develop"); got != "" {
		t.Fatalf("want empty (self-base), got %q", got)
	}
}

// TestResolveBaseBranch_FallsBackWhenUpstreamIsSelf covers a pushed
// feature branch: `git push -u` sets `@{upstream}` to
// `origin/feature/KON-1`, so baseBranchOf returns the branch itself.
// That is not a base — the `b != newBranch` guard must discard it and
// take the main-worktree fallback to `develop`. Without the guard the
// branch degraded to `seed:empty` (parent resolved to its own active
// DB, which fill skips).
func TestResolveBaseBranch_FallsBackWhenUpstreamIsSelf(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v1")
	gitRun(t, repo, "branch", "feature/KON-1")
	gitRun(t, repo, "checkout", "-q", "feature/KON-1")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v2")
	gitRun(t, repo, "checkout", "-q", "develop")
	// Simulate `git push -u`: the branch tracks its OWN remote branch.
	gitRun(t, repo, "remote", "add", "origin", repo)
	gitRun(t, repo, "config", "branch.feature/KON-1.remote", "origin")
	gitRun(t, repo, "config", "branch.feature/KON-1.merge", "refs/heads/feature/KON-1")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	got := resolveBaseBranch(ctx, st, repo, 0, "feature/KON-1")
	if got != "develop" {
		t.Fatalf("want develop (self-upstream discarded, main-wt fallback), got %q", got)
	}
}

// TestResolveBaseSourceDB_FallsBackToMainWhenBaseNotCheckedOut covers the
// dev-box layout where the repo root is parked on an unrelated feature
// branch and the resolved base (`develop`) exists only as a ref — checked
// out in no worktree and not at the repo root. The branch_scoped app DB has
// no `dump.path`, so without a fallback the new worktree cold-seeds an empty
// schema. Since the new branch shares history with the main checkout's
// branch, the branch-agnostic main DB (`kontainer`) is a valid seed.
func TestResolveBaseSourceDB_FallsBackToMainWhenBaseNotCheckedOut(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v1")
	// The new worktree's branch, off develop, tracking origin/develop.
	gitRun(t, repo, "branch", "feature/KON-1")
	// A sibling feature branch that the repo root is parked on.
	gitRun(t, repo, "checkout", "-q", "-b", "feature/KON-2")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v2")
	// feature/KON-1's upstream points at develop (tier-1 base = develop),
	// but develop is checked out nowhere — repo root sits on feature/KON-2.
	gitRun(t, repo, "remote", "add", "origin", repo)
	gitRun(t, repo, "config", "branch.feature/KON-1.remote", "origin")
	gitRun(t, repo, "config", "branch.feature/KON-1.merge", "refs/heads/develop")

	cfg := &config.Config{
		Databases: []config.DatabaseConfig{
			{Engine: "mysql", NameTemplate: "kontainer_{slug}", BranchScoped: true},
		},
		MainWorktree: config.MainWorktreeConfig{
			Databases: []config.DatabaseOverlay{
				{NameTemplate: "kontainer"},
			},
		},
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repo, "test-repo")
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	name, ok, err := resolveBaseSourceDB(ctx, st, cfg, repo, repoID, 0, scopeName, "feature/KON-1")
	if err != nil {
		t.Fatalf("resolveBaseSourceDB: %v", err)
	}
	if !ok || name != "kontainer" {
		t.Fatalf("want main fallback seed 'kontainer', got (%q, %v)", name, ok)
	}
}

// TestResolveBaseSourceDB_EmptyForOrphanWhenBaseNotCheckedOut guards the
// fallback's shared-history check: an orphan branch with no common ancestor
// with the main checkout must still resolve to no source (empty seed), never
// inherit the main DB.
func TestResolveBaseSourceDB_EmptyForOrphanWhenBaseNotCheckedOut(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "t@t")
	gitRun(t, repo, "config", "user.name", "t")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "v1")
	gitRun(t, repo, "checkout", "-q", "--orphan", "orphan")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "orphan-v1")
	// Repo root back on develop so the fallback's main branch is develop
	// (not the orphan itself) and the merge-base guard is what rejects it.
	gitRun(t, repo, "checkout", "-q", "develop")

	cfg := &config.Config{
		Databases: []config.DatabaseConfig{
			{Engine: "mysql", NameTemplate: "kontainer_{slug}", BranchScoped: true},
		},
		MainWorktree: config.MainWorktreeConfig{
			Databases: []config.DatabaseOverlay{{NameTemplate: "kontainer"}},
		},
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repo, "test-repo")
	if err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	_, ok, err := resolveBaseSourceDB(ctx, st, cfg, repo, repoID, 0, scopeName, "orphan")
	if err != nil {
		t.Fatalf("resolveBaseSourceDB: %v", err)
	}
	if ok {
		t.Fatalf("want no source for orphan branch, got a seed")
	}
}

func TestRenderMainBaseDBUsesOverlay(t *testing.T) {
	cfg := &config.Config{
		Databases: []config.DatabaseConfig{
			{Engine: "mysql", NameTemplate: "kontainer_{slug}"},
		},
		MainWorktree: config.MainWorktreeConfig{
			Databases: []config.DatabaseOverlay{
				{NameTemplate: "kontainer"}, // main checkout uses the unprefixed DB
			},
		},
	}
	name, ok, err := renderMainBaseDB(cfg, 0, scopeName, filepath.Join(t.TempDir(), "repo"), "develop")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || name != "kontainer" {
		t.Errorf("expected overlay to resolve to unprefixed 'kontainer', got (%q, %v)", name, ok)
	}
	// The base config must remain untouched by the overlay merge.
	if cfg.Databases[0].NameTemplate != "kontainer_{slug}" {
		t.Errorf("overlay merge mutated base config: %q", cfg.Databases[0].NameTemplate)
	}
}
