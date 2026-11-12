package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
  create-before-engines:
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
  create-before-engines:
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
  create-before-engines:
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
	// `file-change` trigger now lives per-database (covered by
	// TestPerDBOnFileChange below).
	d := writeTmp(t, `
repo: x
hooks:
  create-before-engines:
    - run: "composer install"
  create-after-engines:
    - run: "warm-cache.sh"
  delete-before-engines:
    - run: "drain-queues"
  delete-after-engines:
    - run: "notify-slack"
  checkout:
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
		{"create-before-engines", cfg.Hooks.OnCreateBeforeEngines, "composer install"},
		{"create-after-engines", cfg.Hooks.OnCreateAfterEngines, "warm-cache.sh"},
		{"delete-before-engines", cfg.Hooks.OnDeleteBeforeEngines, "drain-queues"},
		{"delete-after-engines", cfg.Hooks.OnDeleteAfterEngines, "notify-slack"},
		{"checkout", cfg.Hooks.OnCheckout, "refresh-test-data"},
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
// hooks.file-change block parses with the three valid `match:`
// shapes: omitted (wildcard), single string, and list.
func TestOnFileChangeGlobalWithLabels(t *testing.T) {
	d := writeTmp(t, `
repo: x
databases:
  - engine: mysql
    name_template: "app_{slug}"
    inputs:
      - { glob: "db/migrations/**/*.sql", label: migrations }
      - { glob: "db/seeders/**/*.sql",    label: seeders }
  - engine: elasticsearch
    key_prefix: "app_{slug}_"
    inputs:
      - { glob: "es/mappings/**/*.json", label: es-mappings }
hooks:
  file-change:
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
		t.Fatalf("file-change: want 4 actions, got %d", len(cfg.Hooks.OnFileChange))
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
  create-before-engines:
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

func TestActionRejectsRemovedAndUnknownKeys(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"engine: podman", "rename it to `container_engine:`"},
		{"in_container: app", "rename it to `container:`"},
		{"steps: [a]", "use `run:` with a list of strings"},
		{"background: true", "hooks are always async"},
		{"container_net: backend", `unknown key "container_net" (expected one of: run, continue_on_error, cwd, container, compose_service, compose_project, container_engine, container_network)`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			d := writeTmp(t, "repo: x\nhooks:\n  create-before-engines:\n    - run: \"composer install\"\n      "+tc.key+"\n")
			_, err := LoadLayered(d)
			if err == nil {
				t.Fatalf("%s: want error", tc.key)
			}
			if !strings.Contains(err.Error(), "action (line 5)") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: error %q should locate the key and say %q", tc.key, err, tc.want)
			}
		})
	}
}

func TestFileChangeActionRejectsUnknownKeyButAcceptsMatch(t *testing.T) {
	var ok FilteredAction
	if err := yaml.Unmarshal([]byte("match: [a, b]\nrun: x\ncontainer_network: backend"), &ok); err != nil {
		t.Fatal(err)
	}
	if len(ok.Match) != 2 || ok.Network != "backend" {
		t.Fatalf("filtered action: %+v", ok)
	}
	var bad FilteredAction
	err := yaml.Unmarshal([]byte("match: a\nrun: x\nengine: podman"), &bad)
	if err == nil || !strings.Contains(err.Error(), "container_engine") {
		t.Fatalf("legacy engine key under file-change: %v", err)
	}
}

func TestActionContainerRefKeys(t *testing.T) {
	d := writeTmp(t, `
repo: x
hooks:
  create-before-engines:
    - compose_service: app
      compose_project: myproj
      container_engine: podman
      container_network: backend
      run: "composer install"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	want := ContainerRef{ComposeService: "app", ComposeProject: "myproj", ContainerEngine: "podman", Network: "backend"}
	if got := cfg.Hooks.OnCreateBeforeEngines[0].ContainerRef; got != want {
		t.Fatalf("ContainerRef = %+v, want %+v", got, want)
	}
}

func TestActionContainerAndComposeServiceExclusive(t *testing.T) {
	var a Action
	err := yaml.Unmarshal([]byte("run: x\ncontainer: a\ncompose_service: b"), &a)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutual-exclusion error, got %v", err)
	}
}

func TestActionComposeService(t *testing.T) {
	d := writeTmp(t, `
repo: x
hooks:
  create-before-engines:
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
	if a.ComposeService != "app" || a.ComposeProject != "myproj" || a.ContainerEngine != "podman" {
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
  create-before-engines:
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
  create-before-engines:
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
  create-before-engines:
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
  create-before-engines:
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
  create-before-engines:
    - run: "composer install --no-interaction"
    - run: "yarn install --frozen-lockfile"
    - run:
        - "yarn install"
        - "yarn build"
      cwd: frontend
  create-after-engines:
    - run: "warm-cache.sh"
  delete-before-engines:
    - run: "drain-queues"
`)
	cfg, err := LoadLayered(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks.OnCreateBeforeEngines) != 3 {
		t.Errorf("want 3 create-before-engines actions, got %d", len(cfg.Hooks.OnCreateBeforeEngines))
	}
	if len(cfg.Hooks.OnCreateAfterEngines) != 1 {
		t.Errorf("want 1 create-after-engines action, got %d", len(cfg.Hooks.OnCreateAfterEngines))
	}
	if len(cfg.Hooks.OnDeleteBeforeEngines) != 1 {
		t.Errorf("want 1 delete-before-engines action, got %d", len(cfg.Hooks.OnDeleteBeforeEngines))
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
	// Spot-check that the 3rd create-before-engines action is the grouped one with cwd.
	a := cfg.Hooks.OnCreateBeforeEngines[2]
	if a.Cwd != "frontend" || len(a.Run) != 2 {
		t.Errorf("3rd action: %#v", a)
	}
}
