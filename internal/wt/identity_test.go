package wt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// TestResolveIdentityLinkedWtPath covers a normal linked worktree
// (wtPath != repoPath): slug.For path, EnsureWorktree, no overlay.
// This is the dominant case and the helper must not regress it while
// serving the main-wt path.
func TestResolveIdentityLinkedWtPath(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(tmp, "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, err := s.EnsureRepo(ctx, "/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}

	id, err := ResolveIdentity(ctx, s, cfg, "/repo", "/repo/.worktrees/feat", "feat", repoID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id.IsMain {
		t.Errorf("linked-wt path classified as main")
	}
	if id.Slug.Value == "" || id.WtID == 0 {
		t.Errorf("empty slug or wtID: sl=%q wtID=%d", id.Slug.Value, id.WtID)
	}
	row, _ := s.LookupMainWorktree(ctx, repoID)
	if row.ID != 0 {
		t.Errorf("linked-wt should not produce a main row, got %+v", row)
	}
}

// TestResolveIdentityMainWtNoOverlay covers the bare-bones main-wt
// enable case: enabled=true but no databases[] overlay. Helper must
// produce slug.ForMain ("main_<branch>"), EnsureMainWorktree
// (is_main=1), and leave cfg.Databases untouched since there's no
// overlay to apply.
func TestResolveIdentityMainWtNoOverlay(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	mkRepoOnBranch(t, repo, "develop")

	s, err := store.Open(ctx, filepath.Join(tmp, "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, repo, "repo")
	cfg := &config.Config{
		MainWorktree: config.MainWorktreeConfig{Enabled: true},
		Databases: []config.DatabaseConfig{
			{Engine: "mysql", NameTemplate: "app_{slug}"},
		},
	}

	id, err := ResolveIdentity(ctx, s, cfg, repo, repo, "develop", repoID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !id.IsMain {
		t.Fatal("expected isMain=true")
	}
	if id.Slug.Value != "main_develop" {
		t.Errorf("slug=%q want main_develop", id.Slug.Value)
	}
	if cfg.Databases[0].NameTemplate != "app_{slug}" {
		t.Errorf("base template should be untouched without overlay, got %q",
			cfg.Databases[0].NameTemplate)
	}
	row, _ := s.LookupMainWorktree(ctx, repoID)
	if row.ID != id.WtID || !row.IsMain || row.Slug != "main_develop" {
		t.Errorf("main row mismatch: %+v", row)
	}
}

// TestResolveIdentityMainWtPartialOverlay: overlay touches only one
// of two databases (sparse). The first DB must pick up the override;
// the second must inherit from base unchanged.
func TestResolveIdentityMainWtPartialOverlay(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	mkRepoOnBranch(t, repo, "main")

	s, err := store.Open(ctx, filepath.Join(tmp, "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, repo, "repo")
	cfg := &config.Config{
		MainWorktree: config.MainWorktreeConfig{
			Enabled: true,
			Databases: []config.DatabaseOverlay{
				{NameTemplate: "app_dev_{slug}"},
			},
		},
		Databases: []config.DatabaseConfig{
			{Engine: "mysql", NameTemplate: "app_{slug}"},
			{Engine: "postgres", NameTemplate: "analytics_{slug}"},
		},
	}

	id, err := ResolveIdentity(ctx, s, cfg, repo, repo, "main", repoID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !id.IsMain {
		t.Fatal("expected isMain=true")
	}
	if cfg.Databases[0].NameTemplate != "app_dev_{slug}" {
		t.Errorf("db[0] template = %q, want app_dev_{slug} (overlay miss)",
			cfg.Databases[0].NameTemplate)
	}
	if cfg.Databases[1].NameTemplate != "analytics_{slug}" {
		t.Errorf("db[1] template = %q, want analytics_{slug} (sparse overlay leaked)",
			cfg.Databases[1].NameTemplate)
	}
}

// TestResolveIdentityMainWtFullOverlay: overlay touches both
// databases including TestClones replacement. Catches a regression
// where TestClones full-spec replacement gets accidentally merged
// from base when only NameTemplate is supposed to.
func TestResolveIdentityMainWtFullOverlay(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	mkRepoOnBranch(t, repo, "main")

	s, err := store.Open(ctx, filepath.Join(tmp, "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, repo, "repo")
	zero := uint32(0)
	cfg := &config.Config{
		MainWorktree: config.MainWorktreeConfig{
			Enabled: true,
			Databases: []config.DatabaseOverlay{
				{
					NameTemplate: "app_dev_{slug}",
					TestClones:   &config.TestClonesSpec{Clones: config.ClonesSetting{Fixed: 0}},
					Fanout:       &zero,
				},
				{NameTemplate: "analytics_dev_{slug}"},
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "app_{slug}",
				TestClones:   &config.TestClonesSpec{Clones: config.ClonesSetting{Fixed: 8}, NameTemplate: "app_{slug}_t{n}"},
				Fanout:       8,
			},
			{Engine: "postgres", NameTemplate: "analytics_{slug}"},
		},
	}

	id, err := ResolveIdentity(ctx, s, cfg, repo, repo, "main", repoID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !id.IsMain {
		t.Fatal("expected isMain=true")
	}
	if cfg.Databases[0].NameTemplate != "app_dev_{slug}" ||
		cfg.Databases[0].TestClones == nil ||
		cfg.Databases[0].TestClones.Clones.Fixed != 0 ||
		cfg.Databases[0].Fanout != 0 {
		t.Errorf("db[0] full-overlay mismatch: name=%q clones=%+v fanout=%d",
			cfg.Databases[0].NameTemplate,
			cfg.Databases[0].TestClones,
			cfg.Databases[0].Fanout)
	}
	if cfg.Databases[1].NameTemplate != "analytics_dev_{slug}" {
		t.Errorf("db[1] template = %q, want analytics_dev_{slug}",
			cfg.Databases[1].NameTemplate)
	}
}

// TestResolveIdentityRowFallback covers the disable→reload race
// window: cfg.MainWorktree.Enabled is false but an active is_main=1
// row still exists. The helper MUST keep using slug.ForMain so an
// in-flight event can't write a path-hash-keyed DB and orphan the
// per-branch DBs the main slug owns.
func TestResolveIdentityRowFallback(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	mkRepoOnBranch(t, repo, "main")

	s, err := store.Open(ctx, filepath.Join(tmp, "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, repo, "repo")
	if _, err := s.EnsureMainWorktree(ctx, repoID, repo, "main_old", "main"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		MainWorktree: config.MainWorktreeConfig{Enabled: false},
	}

	id, err := ResolveIdentity(ctx, s, cfg, repo, repo, "main", repoID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !id.IsMain {
		t.Fatal("expected isMain=true via row fallback")
	}
	if id.Slug.Value != "main_main" {
		t.Errorf("slug=%q want main_main", id.Slug.Value)
	}
	row, _ := s.LookupMainWorktree(ctx, repoID)
	if row.Slug != "main_main" {
		t.Errorf("row slug = %q, want main_main (path-hash leaked through)", row.Slug)
	}
}

// mkRepoOnBranch initialises a git repo at path with a single empty
// commit on the named branch.
func mkRepoOnBranch(t *testing.T, path, branch string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", branch, path},
	} {
		c := exec.Command("git", args...)
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = path
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}
