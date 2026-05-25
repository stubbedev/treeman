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
repo: x
hooks:
  on-create-before-engines:
    - run: "composer install"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks.OnCreateBeforeEngines) != 1 {
		t.Fatalf("want 1 action, got %d", len(cfg.Hooks.OnCreateBeforeEngines))
	}
	a := cfg.Hooks.OnCreateBeforeEngines[0]
	if len(a.Run) != 1 || a.Run[0] != "composer install" {
		t.Errorf("unexpected: %#v", a)
	}
}

func TestActionListRun(t *testing.T) {
	d := writeTmp(t, `
repo: x
hooks:
  on-create-before-engines:
    - run:
        - "npm install"
        - "npm run build"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Hooks.OnCreateBeforeEngines[0]
	if len(a.Run) != 2 || a.Run[0] != "npm install" || a.Run[1] != "npm run build" {
		t.Errorf("unexpected: %#v", a)
	}
}

func TestActionGroupLevelCwd(t *testing.T) {
	d := writeTmp(t, `
repo: x
hooks:
  on-create-before-engines:
    - run:
        - "yarn install"
        - "yarn build"
      cwd: frontend
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Hooks.OnCreateBeforeEngines[0]
	if a.Cwd != "frontend" {
		t.Errorf("cwd: %q", a.Cwd)
	}
}

func TestAllTriggersParseAndCarryActions(t *testing.T) {
	// Asserts every top-level trigger key in the schema accepts an
	// actions list and lands on the matching struct field. The
	// `on-file-change` trigger now lives per-database (covered by
	// TestPerDBOnFileChange below).
	d := writeTmp(t, `
repo: x
hooks:
  on-create-before-engines:
    - run: "composer install"
  on-create-after-engines:
    - run: "warm-cache.sh"
  on-delete-before-engines:
    - run: "drain-queues"
  on-delete-after-engines:
    - run: "notify-slack"
  on-checkout:
    - run: "refresh-test-data"
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
		{"on-create-before-engines", cfg.Hooks.OnCreateBeforeEngines, "composer install"},
		{"on-create-after-engines", cfg.Hooks.OnCreateAfterEngines, "warm-cache.sh"},
		{"on-delete-before-engines", cfg.Hooks.OnDeleteBeforeEngines, "drain-queues"},
		{"on-delete-after-engines", cfg.Hooks.OnDeleteAfterEngines, "notify-slack"},
		{"on-checkout", cfg.Hooks.OnCheckout, "refresh-test-data"},
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

// TestOnFileChangeGlobalWithLabels asserts the global
// hooks.on-file-change block parses with the three valid `match:`
// shapes: omitted (wildcard), single string, and list.
func TestOnFileChangeGlobalWithLabels(t *testing.T) {
	d := writeTmp(t, `
repo: x
databases:
  - engine: mysql
    name_template: "app_{slug}"
    inputs:
      - { glob: "db/migrations/**/*.sql", label: migrations, hash: filename }
      - { glob: "db/seeders/**/*.sql",    label: seeders }
  - engine: elasticsearch
    key_prefix: "app_{slug}_"
    inputs:
      - { glob: "es/mappings/**/*.json", label: es-mappings }
hooks:
  on-file-change:
    - run: "echo any change anywhere"               # wildcard
    - match: migrations                              # single label
      run: "echo mysql migration changed"
    - match: [migrations, seeders]                   # list of labels
      run: ["echo any mysql change", "echo step two"]
    - match: es-mappings
      run: "curl -XPOST 'http://es/_reindex'"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks.OnFileChange) != 4 {
		t.Fatalf("on-file-change: want 4 actions, got %d", len(cfg.Hooks.OnFileChange))
	}
	a := cfg.Hooks.OnFileChange
	if len(a[0].Match) != 0 {
		t.Errorf("wildcard: want empty Match, got %#v", a[0].Match)
	}
	if !a[0].Matches("anything") || !a[0].Matches("") {
		t.Error("wildcard should match anything")
	}
	if len(a[1].Match) != 1 || a[1].Match[0] != "migrations" {
		t.Errorf("single-string match: %#v", a[1].Match)
	}
	if !a[1].Matches("migrations") || a[1].Matches("seeders") {
		t.Error("single-string match dispatched wrong")
	}
	if len(a[2].Match) != 2 || a[2].Match[0] != "migrations" || a[2].Match[1] != "seeders" {
		t.Errorf("list match: %#v", a[2].Match)
	}
	if !a[2].Matches("migrations") || !a[2].Matches("seeders") || a[2].Matches("es-mappings") {
		t.Error("list match dispatched wrong")
	}
	if !a[3].Matches("es-mappings") {
		t.Error("es-mappings should match")
	}
}

func TestActionContainerSingleStep(t *testing.T) {
	d := writeTmp(t, `
repo: x
hooks:
  on-create-before-engines:
    - run: "composer install"
      container: app
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Hooks.OnCreateBeforeEngines[0]
	if a.Container != "app" {
		t.Errorf("Container=%q want app", a.Container)
	}
}

// TestActionInContainerAliasRejected asserts the removed `in_container:`
// alias produces a clear error rather than silently mis-parsing.
func TestActionInContainerAliasRejected(t *testing.T) {
	d := writeTmp(t, `
repo: x
hooks:
  on-create-before-engines:
    - run: "composer install"
      in_container: app
`)
	_, err := LoadLayered(d)
	if err == nil {
		t.Fatal("want error for removed in_container alias")
	}
}

func TestActionComposeService(t *testing.T) {
	d := writeTmp(t, `
repo: x
hooks:
  on-create-before-engines:
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
	a := cfg.Hooks.OnCreateBeforeEngines[0]
	if a.ComposeService != "app" || a.ComposeProject != "myproj" || a.Engine != "podman" {
		t.Errorf("meta: %#v", a)
	}
	if len(a.Run) != 2 {
		t.Errorf("steps: %d", len(a.Run))
	}
}

func TestActionContainerAndComposeServiceMutuallyExclusive(t *testing.T) {
	d := writeTmp(t, `
repo: x
hooks:
  on-create-before-engines:
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
repo: x
hooks:
  on-create-before-engines:
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
repo: x
hooks:
  on-create-before-engines:
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
repo: x
hooks:
  on-create-before-engines:
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
repo: myapp
worktrees:
  root: .worktrees
  copies: [.env]
  links: [vendor]
env_sources: [.env, .env.testing]
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
  on-create-before-engines:
    - run: "composer install --no-interaction"
    - run: "yarn install --frozen-lockfile"
    - run:
        - "yarn install"
        - "yarn build"
      cwd: frontend
  on-create-after-engines:
    - run: "warm-cache.sh"
  on-delete-before-engines:
    - run: "drain-queues"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks.OnCreateBeforeEngines) != 3 {
		t.Errorf("want 3 on-create-before-engines actions, got %d", len(cfg.Hooks.OnCreateBeforeEngines))
	}
	if len(cfg.Hooks.OnCreateAfterEngines) != 1 {
		t.Errorf("want 1 on-create-after-engines action, got %d", len(cfg.Hooks.OnCreateAfterEngines))
	}
	if len(cfg.Hooks.OnDeleteBeforeEngines) != 1 {
		t.Errorf("want 1 on-delete-before-engines action, got %d", len(cfg.Hooks.OnDeleteBeforeEngines))
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
	// Spot-check that the 3rd on-create-before-engines action is the grouped one with cwd.
	a := cfg.Hooks.OnCreateBeforeEngines[2]
	if a.Cwd != "frontend" || len(a.Run) != 2 {
		t.Errorf("3rd action: %#v", a)
	}
}
