package prepare

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
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
