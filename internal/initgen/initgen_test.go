package initgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/schema"
)

func TestRenderTemplateContainsModeline(t *testing.T) {
	dir := t.TempDir()
	body := RenderTemplate(dir)
	if !strings.Contains(body, "$schema="+schema.URL) {
		t.Errorf("missing schema modeline:\n%s", body)
	}
	if !strings.Contains(body, "worktrees:") {
		t.Errorf("missing worktrees block:\n%s", body)
	}
}

func TestRenderGlobalTemplateScopeAndParse(t *testing.T) {
	body := RenderGlobalTemplate()

	// Parses as a full config.Config (global keys are a valid subset).
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("global template does not parse as config.Config: %v\n%s", err, body)
	}

	// Contains global-scoped blocks.
	for _, k := range []string{"daemon:", "snapshots:", "logs:", "auto_fetch:", "notifications:"} {
		if !strings.Contains(body, k) {
			t.Errorf("global template missing %q:\n%s", k, body)
		}
	}
	// Omits repo-only blocks.
	for _, k := range []string{"databases:", "patches:", "hooks:", "main_worktree:", "env_sources:"} {
		if strings.Contains(body, k) {
			t.Errorf("global template should not contain repo-only %q:\n%s", k, body)
		}
	}
	// Sanity: scalars parsed as their YAML types, not strings.
	if cfg.Daemon.LogLevel != "info" {
		t.Errorf("daemon.log_level = %q, want info", cfg.Daemon.LogLevel)
	}
	if cfg.AutoFetch.Enabled == nil || !*cfg.AutoFetch.Enabled {
		t.Errorf("auto_fetch.enabled should parse as bool true")
	}
}

func TestWriteGlobalYAMLRefusesOverwriteWithoutForce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, created, _, err := WriteGlobalYAML(false); err != nil || !created {
		t.Fatalf("first WriteGlobalYAML: created=%v err=%v", created, err)
	}
	if _, _, _, err := WriteGlobalYAML(false); err == nil {
		t.Errorf("expected error overwriting existing global config without force")
	}
}

func TestRenderTemplateEmitsGroupForGoMod(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module foo\n"), 0o644)
	body := RenderTemplate(dir)
	if !strings.Contains(body, "go mod download") {
		t.Errorf("missing go mod download hook:\n%s", body)
	}
}

func TestRenderTemplateEmitsNpmGroup(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}\n"), 0o644)
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
	_ = os.WriteFile(target, []byte("existing\n"), 0o644)
	_, created, _, err := WriteYAML(dir, false)
	if err == nil {
		t.Errorf("WriteYAML(force=false) should error when file exists (created=%v)", created)
	}
}

func TestWriteYAMLOverwritesWithForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".treeman.yaml")
	_ = os.WriteFile(target, []byte("existing\n"), 0o644)
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
// collapsed declarative schema: against a minimal Laravel-shaped fixture
// (artisan marker + composer.json + database/migrations dir), the scaffold
// should emit a top-level `migrate:` block on the db, an `inputs:` list
// covering both migration globs and the lockfile, and no `framework:`,
// `migrations:`, or `watcher:` blocks.
//
//nolint:cyclop // end-to-end scaffolding test: many small assertions on one fixture is exactly the shape this needs; splitting would just spread the same conditions across helper functions
func TestRenderTemplateLaravelEndToEnd(t *testing.T) {
	dir := t.TempDir()
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
	must("database/migrations/2024_01_01_000000_init.php", "<?php\n")

	body := RenderTemplate(dir)

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("rendered template is not valid YAML: %v\n%s", err, body)
	}

	dbs, ok := doc["databases"].([]any)
	if !ok || len(dbs) == 0 {
		t.Fatalf("databases: missing or wrong type\n%s", body)
	}
	db, _ := dbs[0].(map[string]any)
	if got := db["engine"]; got != "mysql" {
		t.Errorf("engine = %v, want mysql", got)
	}
	if _, has := db["migrations"]; has {
		t.Errorf("databases[0].migrations should not exist after collapse refactor; got %v", db["migrations"])
	}
	if _, has := doc["watcher"]; has {
		t.Errorf("top-level watcher: block should not exist after collapse refactor; got %v", doc["watcher"])
	}

	migrate, ok := db["migrate"].(map[string]any)
	if !ok {
		t.Fatalf("databases[0].migrate: missing or wrong type\n%s", body)
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

	// Laravel has a step-based rollback CLI, so a rollback block must be
	// scaffolded referencing the injected step-count env var.
	rollback, ok := db["rollback"].(map[string]any)
	if !ok {
		t.Fatalf("databases[0].rollback: missing or wrong type\n%s", body)
	}
	if got := rollback["run"]; got != "php artisan migrate:rollback --force --step=$TREEMAN_ROLLBACK_STEPS" {
		t.Errorf("rollback.run = %v, want laravel rollback default", got)
	}
	rbEnv, ok := rollback["env"].(map[string]any)
	if !ok {
		t.Fatalf("rollback.env: missing or wrong type\n%s", body)
	}
	if got := rbEnv["DB_DATABASE"]; got != "{target_db}" {
		t.Errorf("rollback.env.DB_DATABASE = %v, want {target_db}", got)
	}

	// Round-trip through the real config loader: the scaffolded rollback
	// block must bind to DatabaseConfig.Rollback, not just be loose YAML.
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("scaffolded yaml fails to load as config: %v\n%s", err, body)
	}
	if len(cfg.Databases) == 0 || cfg.Databases[0].Rollback == nil {
		t.Fatalf("DatabaseConfig.Rollback not populated from scaffold\n%s", body)
	}
	if cfg.Databases[0].Rollback.Run != "php artisan migrate:rollback --force --step=$TREEMAN_ROLLBACK_STEPS" {
		t.Errorf("bound Rollback.Run = %q", cfg.Databases[0].Rollback.Run)
	}

	inputs, ok := db["inputs"].([]any)
	if !ok || len(inputs) == 0 {
		t.Fatalf("databases[0].inputs: missing or empty\n%s", body)
	}
	type entry struct{ label string }
	wantInputs := map[string]entry{
		"database/migrations/**/*.php":               {label: "migrations"},
		"app/Modules/*/Database/Migrations/**/*.php": {label: "migrations"},
		"app/Modules/*/Database/migrations/**/*.php": {label: "migrations"},
		"Modules/*/Database/Migrations/**/*.php":     {label: "migrations"},
		"Modules/*/Database/migrations/**/*.php":     {label: "migrations"},
		"composer.lock":                              {label: "lockfile"},
	}
	gotInputs := map[string]entry{}
	for _, in := range inputs {
		m, _ := in.(map[string]any)
		g, _ := m["glob"].(string)
		lbl, _ := m["label"].(string)
		gotInputs[g] = entry{label: lbl}
	}
	// Scaffolded YAML must not carry a `hash:` field anymore (filename
	// mode removed; all inputs are content-hashed). A single raw-body
	// substring check covers every input at once.
	if strings.Contains(body, "hash:") {
		t.Errorf("scaffolded yaml leaks a `hash:` field; expected none:\n%s", body)
	}
	for g, want := range wantInputs {
		got, ok := gotInputs[g]
		if !ok {
			t.Errorf("inputs[%q]: missing", g)
			continue
		}
		if got != want {
			t.Errorf("inputs[%q] = %+v, want %+v", g, got, want)
		}
	}
}

// TestRenderTemplateNoRollbackForFrameworkWithoutStepDown verifies that
// a framework lacking a clean step-based rollback CLI (Prisma — forward-
// only `migrate deploy`, no relative down) scaffolds NO rollback block,
// so those projects fall back to cold rebuild on an edited migration.
func TestRenderTemplateNoRollbackForFrameworkWithoutStepDown(t *testing.T) {
	dir := t.TempDir()
	must := func(p, body string) {
		t.Helper()
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("prisma/schema.prisma", "generator client {}\n")
	must("package-lock.json", `{"name":"acme"}`)
	must("prisma/migrations/20240101000000_init/migration.sql", "CREATE TABLE t (id INT);\n")

	body := RenderTemplate(dir)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("rendered template is not valid YAML: %v\n%s", err, body)
	}
	dbs, ok := doc["databases"].([]any)
	if !ok || len(dbs) == 0 {
		t.Fatalf("databases: missing\n%s", body)
	}
	db, _ := dbs[0].(map[string]any)
	if _, has := db["rollback"]; has {
		t.Errorf("prisma must NOT scaffold a rollback block; got %v", db["rollback"])
	}
}

func TestDetectJSPkgMgrPrecedence(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "yarn.lock"), []byte("\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte("\n"), 0o644)
	// pnpm beats yarn
	if got := detectJSPkgMgr(dir); got != "pnpm" {
		t.Errorf("detectJSPkgMgr = %q, want pnpm", got)
	}
}
