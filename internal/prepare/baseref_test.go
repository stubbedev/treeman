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
	defer st.Close()

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
	defer st.Close()
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
	defer st.Close()
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
	defer st.Close()
	if got := resolveBaseBranch(ctx, st, repo, 0, "develop"); got != "" {
		t.Fatalf("want empty (self-base), got %q", got)
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
