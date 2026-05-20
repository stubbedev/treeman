package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, content string) string {
	t.Helper()
	// Isolate the test from the user's real ~/.config/treeman/
	// config.yaml so config-merge layering doesn't pick it up.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	d := t.TempDir()
	p := filepath.Join(d, ".treeman.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseBareStringHookEntry(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  postcreate:
    - "composer install"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks.Postcreate) != 1 {
		t.Fatalf("want 1 group, got %d", len(cfg.Hooks.Postcreate))
	}
	g := cfg.Hooks.Postcreate[0]
	if len(g.Steps) != 1 || g.Steps[0].Run != "composer install" {
		t.Errorf("unexpected: %#v", g)
	}
}

func TestParseMapHookEntry(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  postcreate:
    - { run: "yarn install", cwd: frontend }
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Hooks.Postcreate[0]
	if g.Steps[0].Run != "yarn install" || g.Steps[0].Cwd != "frontend" {
		t.Errorf("unexpected: %#v", g)
	}
}

func TestParseGroupHookEntry(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  postcreate:
    - - "npm install"
      - { run: "npm run build" }
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Hooks.Postcreate[0]
	if len(g.Steps) != 2 {
		t.Fatalf("want 2 steps in group, got %d", len(g.Steps))
	}
	if g.Steps[0].Run != "npm install" || g.Steps[1].Run != "npm run build" {
		t.Errorf("unexpected: %#v", g)
	}
}

func TestLegacyBackgroundFieldRejected(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  postcreate:
    - { run: "yarn install", background: true }
`)
	_, err := LoadLayered(d)
	if err == nil {
		t.Fatal("want error rejecting legacy `background:` field")
	}
	if !strings.Contains(err.Error(), "background") {
		t.Errorf("error should mention `background`: %v", err)
	}
}

func TestKontainerStyleConfig(t *testing.T) {
	// Sanity check: a realistic Laravel-style YAML parses to the
	// expected shape. Mirrors the new kontainer .treeman.yaml.
	d := writeTmp(t, `
repo:
  name: myapp
worktrees:
  root: .worktrees
  links: [.env, .env.testing]
env_scoping:
  files: [.env.testing, phpunit.xml]
  skip_worktree: true
  patches:
    - { key: DB_TEST_DATABASE, template: "myapp_testing_{slug}" }
databases:
  - engine: mysql
    name_template: "myapp_testing_{slug}"
    paratest:
      clones: auto
      name_template: "myapp_testing_{slug}_test_{n}"
hooks:
  postcreate:
    - "composer install --no-interaction"
    - "yarn install --frozen-lockfile"
    - { run: "yarn install", cwd: frontend }
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo == nil || cfg.Repo.Name != "myapp" {
		t.Errorf("repo: %#v", cfg.Repo)
	}
	if cfg.Worktrees.Root != ".worktrees" {
		t.Errorf("worktrees.root: %q", cfg.Worktrees.Root)
	}
	if cfg.Worktrees.AsyncCreate == nil || !*cfg.Worktrees.AsyncCreate {
		t.Error("async_create default should be true")
	}
	if cfg.Worktrees.AsyncDelete == nil || !*cfg.Worktrees.AsyncDelete {
		t.Error("async_delete default should be true")
	}
	if len(cfg.Hooks.Postcreate) != 3 {
		t.Errorf("want 3 postcreate groups, got %d", len(cfg.Hooks.Postcreate))
	}
	if len(cfg.Databases) != 1 || cfg.Databases[0].Engine != "mysql" {
		t.Errorf("databases: %#v", cfg.Databases)
	}
	if cfg.Databases[0].Paratest == nil || !cfg.Databases[0].Paratest.Clones.Auto {
		t.Error("paratest.clones should be auto")
	}
}
