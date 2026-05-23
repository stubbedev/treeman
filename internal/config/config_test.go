package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".treeman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestActionSingleStringRun(t *testing.T) {
	// `run: "..."` decodes to []string{"..."} so callers don't need
	// to branch on shape.
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - run: "composer install"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks.SetupBeforeEngines) != 1 {
		t.Fatalf("want 1 action, got %d", len(cfg.Hooks.SetupBeforeEngines))
	}
	a := cfg.Hooks.SetupBeforeEngines[0]
	if len(a.Run) != 1 || a.Run[0] != "composer install" {
		t.Errorf("unexpected: %#v", a)
	}
}

func TestActionListRun(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - run:
        - "npm install"
        - "npm run build"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Hooks.SetupBeforeEngines[0]
	if len(a.Run) != 2 || a.Run[0] != "npm install" || a.Run[1] != "npm run build" {
		t.Errorf("unexpected: %#v", a)
	}
}

func TestActionGroupLevelCwd(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - run:
        - "yarn install"
        - "yarn build"
      cwd: frontend
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Hooks.SetupBeforeEngines[0]
	if a.Cwd != "frontend" {
		t.Errorf("cwd: %q", a.Cwd)
	}
}

func TestAllTriggersParseAndCarryActions(t *testing.T) {
	// Asserts every trigger key in the schema accepts an actions
	// list and lands on the matching struct field.
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - run: "composer install"
  setup-after-engines:
    - run: "warm-cache.sh"
  teardown-before-engines:
    - run: "drain-queues"
  teardown-after-engines:
    - run: "notify-slack"
  on-head-change:
    - run: "refresh-test-data"
  on-watch:
    - run: "echo seeds changed"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		actual []Action
		want   string
	}{
		{"setup-before-engines", cfg.Hooks.SetupBeforeEngines, "composer install"},
		{"setup-after-engines", cfg.Hooks.SetupAfterEngines, "warm-cache.sh"},
		{"teardown-before-engines", cfg.Hooks.TeardownBeforeEngines, "drain-queues"},
		{"teardown-after-engines", cfg.Hooks.TeardownAfterEngines, "notify-slack"},
		{"on-head-change", cfg.Hooks.OnHeadChange, "refresh-test-data"},
		{"on-watch", cfg.Hooks.OnWatch, "echo seeds changed"},
	}
	for _, c := range cases {
		if len(c.actual) != 1 {
			t.Errorf("%s: want 1 action, got %d", c.name, len(c.actual))
			continue
		}
		if len(c.actual[0].Run) != 1 || c.actual[0].Run[0] != c.want {
			t.Errorf("%s: got %#v, want %q", c.name, c.actual[0].Run, c.want)
		}
	}
}

func TestActionContainerSingleStep(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - run: "composer install"
      in_container: app
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Hooks.SetupBeforeEngines[0]
	if a.Container != "app" {
		t.Errorf("Container=%q want app", a.Container)
	}
}

func TestActionComposeService(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - compose_service: app
      compose_project: myproj
      container_engine: podman
      run:
        - "composer install"
        - "php artisan migrate"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Hooks.SetupBeforeEngines[0]
	if a.ComposeService != "app" || a.ComposeProject != "myproj" || a.Engine != "podman" {
		t.Errorf("meta: %#v", a)
	}
	if len(a.Run) != 2 {
		t.Errorf("steps: %d", len(a.Run))
	}
}

func TestActionContainerAndComposeServiceMutuallyExclusive(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - container: app
      compose_service: also-app
      run: "boom"
`)
	if _, err := LoadLayered(d); err == nil {
		t.Fatal("want error for combined refs")
	}
}

func TestLegacyBackgroundFieldRejected(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - run: "yarn install"
      background: true
`)
	_, err := LoadLayered(d)
	if err == nil {
		t.Fatal("want error rejecting legacy `background:` field")
	}
	if !strings.Contains(err.Error(), "background") {
		t.Errorf("error should mention `background`: %v", err)
	}
}

func TestLegacyStepsKeywordRejected(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - steps:
        - "a"
        - "b"
`)
	_, err := LoadLayered(d)
	if err == nil {
		t.Fatal("want error rejecting legacy `steps:` keyword")
	}
	if !strings.Contains(err.Error(), "steps") {
		t.Errorf("error should mention `steps:`: %v", err)
	}
}

func TestActionRejectsBareString(t *testing.T) {
	d := writeTmp(t, `
repo: { name: x }
hooks:
  setup-before-engines:
    - "composer install"
`)
	_, err := LoadLayered(d)
	if err == nil {
		t.Fatal("want error rejecting bare-string shorthand")
	}
	if !strings.Contains(err.Error(), "mapping") {
		t.Errorf("error should explain the required mapping shape: %v", err)
	}
}

func TestRealisticLaravelStyleConfig(t *testing.T) {
	// Sanity check: a realistic Laravel-style YAML parses to the
	// expected shape — flat trigger-keyed hooks, patches block,
	// databases with paratest fan-out.
	d := writeTmp(t, `
repo:
  name: myapp
worktrees:
  root: .worktrees
  copies: [.env]
  links: [vendor]
env_scoping:
  sources: [.env, .env.testing]
patches:
  - file: .env.testing
    set:
      DB_TEST_DATABASE: "myapp_testing_{slug}"
databases:
  - engine: mysql
    name_template: "myapp_testing_{slug}"
    test_clones:
      clones: auto
      name_template: "myapp_testing_{slug}_test_{n}"
hooks:
  setup-before-engines:
    - run: "composer install --no-interaction"
    - run: "yarn install --frozen-lockfile"
    - run:
        - "yarn install"
        - "yarn build"
      cwd: frontend
  setup-after-engines:
    - run: "warm-cache.sh"
  teardown-before-engines:
    - run: "drain-queues"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo == nil || cfg.Repo.Name != "myapp" {
		t.Errorf("repo: %#v", cfg.Repo)
	}
	if len(cfg.Hooks.SetupBeforeEngines) != 3 {
		t.Errorf("want 3 setup-before-engines actions, got %d", len(cfg.Hooks.SetupBeforeEngines))
	}
	if len(cfg.Hooks.SetupAfterEngines) != 1 {
		t.Errorf("want 1 setup-after-engines action, got %d", len(cfg.Hooks.SetupAfterEngines))
	}
	if len(cfg.Hooks.TeardownBeforeEngines) != 1 {
		t.Errorf("want 1 teardown-before-engines action, got %d", len(cfg.Hooks.TeardownBeforeEngines))
	}
	if len(cfg.Databases) != 1 || cfg.Databases[0].Engine != "mysql" {
		t.Errorf("databases: %#v", cfg.Databases)
	}
	if cfg.Databases[0].TestClones == nil || !cfg.Databases[0].TestClones.Clones.Auto {
		t.Error("test_clones.clones should be auto")
	}
	if len(cfg.Patches) != 1 || cfg.Patches[0].File != ".env.testing" {
		t.Errorf("patches: %#v", cfg.Patches)
	}
	// Spot-check that the 3rd setup-before-engines action is the grouped one with cwd.
	a := cfg.Hooks.SetupBeforeEngines[2]
	if a.Cwd != "frontend" || len(a.Run) != 2 {
		t.Errorf("3rd action: %#v", a)
	}
}
