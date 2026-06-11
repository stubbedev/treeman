package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
)

// writeLaravelMarkers lays down the minimum files for the laravel
// detector to fire under repoRoot.
func writeLaravelMarkers(t *testing.T, repoRoot string) {
	t.Helper()
	write := func(rel, body string) {
		full := filepath.Join(repoRoot, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("artisan", "#!/usr/bin/env php\n")
	write("composer.json", `{"name":"acme/app"}`)
	write("composer.lock", `{"_readme":[]}`)
	write("database/migrations/2024_01_01_000000_init.php", "<?php\n")
}

// TestFwDetectSurfacesRollbackPreset proves the rollback preset is wired
// through the MCP fw_detect tool — an agent inspecting a laravel repo
// sees the rollback command, not just migrate.
func TestFwDetectSurfacesRollbackPreset(t *testing.T) {
	repo := newTempRepo(t)
	writeLaravelMarkers(t, repo)

	_, out, err := fwDetectTool(context.Background(), nil, fwDetectIn{Repo: repo})
	if err != nil {
		t.Fatalf("fw_detect: %v", err)
	}
	var laravel bool
	for _, s := range out.Migration {
		if s.Name != "laravel" {
			continue
		}
		laravel = true
		if !strings.Contains(s.RollbackRun, "migrate:rollback") ||
			!strings.Contains(s.RollbackRun, "TREEMAN_ROLLBACK_STEPS") {
			t.Errorf("laravel RollbackRun not surfaced via fw_detect: %q", s.RollbackRun)
		}
		if s.RollbackEnv["DB_DATABASE"] != "{target_db}" {
			t.Errorf("laravel RollbackEnv not surfaced: %v", s.RollbackEnv)
		}
	}
	if !laravel {
		t.Fatalf("fw_detect did not detect laravel; got %+v", out.Migration)
	}
}

// TestInitRepoScaffoldsRollback proves the MCP init_repo tool writes a
// rollback block that binds to DatabaseConfig.Rollback — confirming the
// preset reaches the scaffolder through the MCP path, not just the unit
// RenderTemplate path.
func TestInitRepoScaffoldsRollback(t *testing.T) {
	repo := newTempRepo(t)
	writeLaravelMarkers(t, repo)

	_, out, err := initRepoTool(context.Background(), nil, initIn{Repo: repo})
	if err != nil {
		t.Fatalf("init_repo: %v", err)
	}
	if !out.Created {
		t.Fatalf("init_repo did not create a config (path=%s)", out.Path)
	}
	body, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("scaffolded config does not load: %v\n%s", err, body)
	}
	if len(cfg.Databases) == 0 || cfg.Databases[0].Rollback == nil {
		t.Fatalf("init_repo scaffold missing databases[0].rollback\n%s", body)
	}
	if !strings.Contains(cfg.Databases[0].Rollback.Run, "TREEMAN_ROLLBACK_STEPS") {
		t.Errorf("scaffolded rollback.run = %q", cfg.Databases[0].Rollback.Run)
	}
}
