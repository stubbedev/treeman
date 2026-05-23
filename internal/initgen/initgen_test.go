package initgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/schema"
)

func TestRenderTemplateContainsModelineAndRepoName(t *testing.T) {
	dir := t.TempDir()
	os.Rename(dir, dir) // no-op — just exercising the path
	body := RenderTemplate(dir)
	if !strings.Contains(body, "$schema="+schema.URL) {
		t.Errorf("missing schema modeline:\n%s", body)
	}
	want := "name: " + filepath.Base(dir)
	if !strings.Contains(body, want) {
		t.Errorf("missing %q:\n%s", want, body)
	}
}

func TestRenderTemplateEmitsGroupForGoMod(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module foo\n"), 0o644)
	body := RenderTemplate(dir)
	if !strings.Contains(body, "go mod download") {
		t.Errorf("missing go mod download hook:\n%s", body)
	}
}

func TestRenderTemplateEmitsNpmGroup(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}\n"), 0o644)
	body := RenderTemplate(dir)
	if !strings.Contains(body, "npm ci") {
		t.Errorf("missing npm ci hook:\n%s", body)
	}
}

func TestRenderTemplateParsesAsYAML(t *testing.T) {
	dir := t.TempDir()
	body := RenderTemplate(dir)
	var doc any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("rendered template is not valid YAML: %v\n%s", err, body)
	}
}

func TestWriteYAMLRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".treeman.yaml")
	os.WriteFile(target, []byte("existing\n"), 0o644)
	_, _, _, err := WriteYAML(dir, false)
	if err == nil {
		t.Error("WriteYAML(force=false) should error when file exists")
	}
}

func TestWriteYAMLOverwritesWithForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".treeman.yaml")
	os.WriteFile(target, []byte("existing\n"), 0o644)
	_, created, body, err := WriteYAML(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("created=true when overwriting existing file")
	}
	if body == "existing\n" {
		t.Error("file not overwritten")
	}
}

// TestRenderTemplateLaravelEndToEnd is the integration check for the
// "first step" refactor: against a minimal Laravel-shaped fixture
// (artisan marker + composer.json + database/migrations dir), the
// scaffold should emit a fully explicit migrate block, watcher.paths
// derived from migration_dirs × file_globs (plus the composer.lock
// rebuild entry), and NO `framework:` line under `migrations:`.
func TestRenderTemplateLaravelEndToEnd(t *testing.T) {
	dir := t.TempDir()
	// Marker files that drive framework.Detect for the laravel preset.
	must := func(p string, body string) {
		t.Helper()
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("artisan", "#!/usr/bin/env php\n")
	must("composer.json", `{"name":"acme/app"}`)
	must("composer.lock", `{"_readme": []}`)
	// Optional but realistic: include one migration so the dir actually exists.
	must("database/migrations/2024_01_01_000000_init.php", "<?php\n")

	body := RenderTemplate(dir)

	// Parse the body so we can assert on the structure rather than on
	// brittle substring matches. The result is map-shaped under each
	// top-level key.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("rendered template is not valid YAML: %v\n%s", err, body)
	}

	// ── migrations: ──
	dbs, ok := doc["databases"].([]any)
	if !ok || len(dbs) == 0 {
		t.Fatalf("databases: missing or wrong type\n%s", body)
	}
	db, _ := dbs[0].(map[string]any)
	if got := db["engine"]; got != "mysql" {
		t.Errorf("engine = %v, want mysql", got)
	}
	mig, ok := db["migrations"].(map[string]any)
	if !ok {
		t.Fatalf("databases[0].migrations: missing or wrong type\n%s", body)
	}
	if _, has := mig["framework"]; has {
		t.Errorf("migrations.framework should be absent after the refactor; got %v", mig["framework"])
	}
	// migrate sub-block: run + env with {target_db} substitution targets.
	migrate, ok := mig["migrate"].(map[string]any)
	if !ok {
		t.Fatalf("migrations.migrate: missing or wrong type\n%s", body)
	}
	if got := migrate["run"]; got != "php artisan migrate --force" {
		t.Errorf("migrate.run = %v, want laravel default", got)
	}
	env, ok := migrate["env"].(map[string]any)
	if !ok {
		t.Fatalf("migrate.env: missing or wrong type\n%s", body)
	}
	if got := env["DB_DATABASE"]; got != "{target_db}" {
		t.Errorf("migrate.env.DB_DATABASE = %v, want {target_db}", got)
	}
	if got := env["DB_TEST_DATABASE"]; got != "{target_db}" {
		t.Errorf("migrate.env.DB_TEST_DATABASE = %v, want {target_db}", got)
	}

	// ── watcher.paths ── derived from migration_dirs × file_globs + lockfiles.
	watcher, ok := doc["watcher"].(map[string]any)
	if !ok {
		t.Fatalf("watcher: missing or wrong type\n%s", body)
	}
	paths, ok := watcher["paths"].([]any)
	if !ok || len(paths) == 0 {
		t.Fatalf("watcher.paths: missing or empty\n%s", body)
	}
	// Expected: one glob per (migration_dir × file_glob) + one lockfile entry.
	wantGlobs := map[string]string{
		"database/migrations/**/*.php":                       "rebuild",
		"app/Modules/*/Database/Migrations/**/*.php":         "rebuild",
		"app/Modules/*/Database/migrations/**/*.php":         "rebuild",
		"Modules/*/Database/Migrations/**/*.php":             "rebuild",
		"Modules/*/Database/migrations/**/*.php":             "rebuild",
		"composer.lock":                                      "rebuild",
	}
	gotGlobs := map[string]string{}
	for _, p := range paths {
		m, _ := p.(map[string]any)
		g, _ := m["glob"].(string)
		on, _ := m["on"].(string)
		gotGlobs[g] = on
	}
	for g, on := range wantGlobs {
		if gotGlobs[g] != on {
			t.Errorf("watcher.paths[%q] = %q, want %q", g, gotGlobs[g], on)
		}
	}
}

func TestDetectJSPkgMgrPrecedence(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "yarn.lock"), []byte("\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte("\n"), 0o644)
	// pnpm beats yarn
	if got := detectJSPkgMgr(dir); got != "pnpm" {
		t.Errorf("detectJSPkgMgr = %q, want pnpm", got)
	}
}
