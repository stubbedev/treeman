package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigHistoryAndRestoreTools exercises the MCP config_history +
// config_restore tools end-to-end against a real store: two config_set
// edits create two generations, history lists them newest-first, and
// restore rolls the file back (and is itself reversible).
func TestConfigHistoryAndRestoreTools(t *testing.T) {
	ctx := context.Background()
	repo := newTempRepo(t)
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "t.db"))

	cfgPath := filepath.Join(repo, ".treeman.yaml")
	if err := os.WriteFile(cfgPath, []byte("worktrees:\n  root: .worktrees\ndebounce_ms: 100\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two edits → two stored generations.
	if _, _, err := configSetTool(ctx, nil, configSetIn{Repo: repo, Path: "debounce_ms", Value: float64(200)}); err != nil {
		t.Fatalf("config_set #1: %v", err)
	}
	if _, _, err := configSetTool(ctx, nil, configSetIn{Repo: repo, Path: "debounce_ms", Value: float64(300)}); err != nil {
		t.Fatalf("config_set #2: %v", err)
	}

	_, hist, err := configHistoryTool(ctx, nil, configHistoryIn{Repo: repo})
	if err != nil {
		t.Fatalf("config_history: %v", err)
	}
	if len(hist.Generations) != 2 {
		t.Fatalf("expected 2 generations, got %d", len(hist.Generations))
	}
	if hist.Generations[0].Generation != 2 || hist.Generations[1].Generation != 1 {
		t.Errorf("expected newest-first [2,1], got [%d,%d]",
			hist.Generations[0].Generation, hist.Generations[1].Generation)
	}

	// Restore generation 1 (the original debounce_ms: 100).
	_, rest, err := configRestoreTool(ctx, nil, configRestoreIn{Repo: repo, Generation: 1})
	if err != nil {
		t.Fatalf("config_restore: %v", err)
	}
	if rest.Restored != 1 {
		t.Errorf("Restored = %d, want 1", rest.Restored)
	}
	body, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(body), "debounce_ms: 100") {
		t.Errorf("restore did not write original content:\n%s", body)
	}

	// Restore is reversible — pre-restore content (debounce_ms: 300) was
	// snapshotted as a new generation, so history now has 3.
	_, hist2, err := configHistoryTool(ctx, nil, configHistoryIn{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(hist2.Generations) != 3 {
		t.Errorf("expected 3 generations after reversible restore, got %d", len(hist2.Generations))
	}

	// Missing generation errors.
	if _, _, err := configRestoreTool(ctx, nil, configRestoreIn{Repo: repo, Generation: 99}); err == nil {
		t.Errorf("expected error restoring nonexistent generation")
	}
}

// TestConfigWriteToolsRejectScopeViolations is the write-time guard:
// config_set and config_write must refuse a global-only key in a repo
// file BEFORE it lands on disk (hard scope break).
func TestConfigWriteToolsRejectScopeViolations(t *testing.T) {
	ctx := context.Background()
	repo := newTempRepo(t)
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "t.db"))
	cfgPath := filepath.Join(repo, ".treeman.yaml")
	orig := "worktrees:\n  root: .worktrees\n"
	if err := os.WriteFile(cfgPath, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	// config_set a global-only key → rejected, file untouched.
	if _, _, err := configSetTool(ctx, nil, configSetIn{Repo: repo, Path: "daemon.log_level", Value: "debug"}); err == nil {
		t.Errorf("config_set daemon.log_level in repo should be rejected")
	}
	// config_write a body with a global-only key → rejected.
	if _, _, err := configWriteTool(ctx, nil, configWriteIn{Repo: repo, Body: "snapshots:\n  max_age_days: 5\n"}); err == nil {
		t.Errorf("config_write with snapshots: should be rejected")
	}
	body, _ := os.ReadFile(cfgPath)
	if string(body) != orig {
		t.Errorf("rejected writes must not modify the file:\n%s", body)
	}
	// A repo-valid key still works.
	if _, _, err := configSetTool(ctx, nil, configSetIn{Repo: repo, Path: "debounce_ms", Value: float64(200)}); err != nil {
		t.Errorf("config_set of repo-valid key failed: %v", err)
	}
}

// TestInitRepoGlobalTool verifies the init_repo global=true path
// scaffolds the user-global config (global-scoped keys only) and
// installs a global-scoped schema beside it.
func TestInitRepoGlobalTool(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	_, out, err := initRepoTool(context.Background(), nil, initIn{Global: true})
	if err != nil {
		t.Fatalf("init_repo global: %v", err)
	}
	if out.Scope != "global" || !out.Created {
		t.Errorf("unexpected out: %+v", out)
	}
	body, err := os.ReadFile(filepath.Join(cfgDir, "treeman", "config.yaml"))
	if err != nil {
		t.Fatalf("global config not written: %v", err)
	}
	bs := string(body)
	if !strings.Contains(bs, "daemon:") {
		t.Errorf("global config missing daemon::\n%s", bs)
	}
	if strings.Contains(bs, "databases:") {
		t.Errorf("global config should not contain repo-only databases::\n%s", bs)
	}
	if out.Schema == "" {
		t.Errorf("expected schema path in output")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "treeman", "treeman.schema.json")); err != nil {
		t.Errorf("global schema not installed: %v", err)
	}

	// Refuses to clobber without force.
	if _, _, err := initRepoTool(context.Background(), nil, initIn{Global: true}); err == nil {
		t.Errorf("expected init_repo global to refuse overwrite without force")
	}
}
