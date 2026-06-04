package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// globalCfgEnv points both the global config dir (XDG_CONFIG_HOME) and
// the SQLite store at temp dirs so a test can write/read/delete the
// user-global config without touching the developer's real ~/.config.
func globalCfgEnv(t *testing.T) string {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "t.db"))
	return filepath.Join(cfgDir, "treeman", "config.yaml")
}

// TestConfigSetGlobal_CreatesAndReads — config_set with scope=global on a
// not-yet-existing file creates it, and config_get scope=global reads the
// value back. Exercises the create + locate + read path for the global
// layer end-to-end.
func TestConfigSetGlobal_CreatesAndReads(t *testing.T) {
	ctx := context.Background()
	gp := globalCfgEnv(t)

	if _, out, err := configSetTool(ctx, nil, configSetIn{Scope: "global", Path: "daemon.log_level", Value: "debug"}); err != nil {
		t.Fatalf("config_set global: %v", err)
	} else if out.Scope != "global" || out.File != gp {
		t.Errorf("unexpected out: %+v (want file %s)", out, gp)
	}
	if _, err := os.Stat(gp); err != nil {
		t.Fatalf("global config not created: %v", err)
	}
	if !strings.Contains(readFile(t, gp), "log_level: debug") {
		t.Errorf("global config missing set value:\n%s", readFile(t, gp))
	}

	_, get, err := configGetTool(ctx, nil, configGetIn{Scope: "global"})
	if err != nil {
		t.Fatalf("config_get global: %v", err)
	}
	if get["scope"] != "global" {
		t.Errorf("config_get scope = %v, want global", get["scope"])
	}
}

// TestConfigGlobal_RejectsRepoOnlyKey — the scope guard must refuse a
// repo-only key (databases) in the global file, mirroring the existing
// repo-side guard.
func TestConfigGlobal_RejectsRepoOnlyKey(t *testing.T) {
	ctx := context.Background()
	globalCfgEnv(t)

	if _, _, err := configSetTool(ctx, nil, configSetIn{Scope: "global", Path: "databases[0].engine", Value: "mysql"}); err == nil {
		t.Errorf("config_set databases in global should be rejected")
	}
	if _, _, err := configWriteTool(ctx, nil, configWriteIn{Scope: "global", Body: "patches:\n  - file: .env\n"}); err == nil {
		t.Errorf("config_write with repo-only patches: in global should be rejected")
	}
}

// TestConfigUnsetGlobal removes a key from the global config and confirms
// it's gone while a sibling key survives.
func TestConfigUnsetGlobal(t *testing.T) {
	ctx := context.Background()
	gp := globalCfgEnv(t)
	mustMkdir(t, filepath.Dir(gp))
	if err := os.WriteFile(gp, []byte("daemon:\n  log_level: debug\n  gc_interval: 5m\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, out, err := configUnsetTool(ctx, nil, configUnsetIn{Scope: "global", Path: "daemon.gc_interval"}); err != nil {
		t.Fatalf("config_unset global: %v", err)
	} else if out.Scope != "global" {
		t.Errorf("scope = %s, want global", out.Scope)
	}
	body := readFile(t, gp)
	if strings.Contains(body, "gc_interval") {
		t.Errorf("gc_interval not removed:\n%s", body)
	}
	if !strings.Contains(body, "log_level: debug") {
		t.Errorf("sibling key log_level wrongly removed:\n%s", body)
	}

	// Removing a missing key errors.
	if _, _, err := configUnsetTool(ctx, nil, configUnsetIn{Scope: "global", Path: "daemon.nope"}); err == nil {
		t.Errorf("expected error unsetting a missing key")
	}
}

// TestConfigDeleteGlobal — config_delete previews without ack and only
// removes the file (recoverably) once ack=true.
func TestConfigDeleteGlobal(t *testing.T) {
	ctx := context.Background()
	gp := globalCfgEnv(t)
	mustMkdir(t, filepath.Dir(gp))
	if err := os.WriteFile(gp, []byte("daemon:\n  log_level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bare call previews, does not delete.
	_, prev, err := configDeleteTool(ctx, nil, configDeleteIn{Scope: "global"})
	if err != nil {
		t.Fatalf("config_delete preview: %v", err)
	}
	if prev.Deleted || !prev.DryRun {
		t.Errorf("bare config_delete should preview, got %+v", prev)
	}
	if _, err := os.Stat(gp); err != nil {
		t.Errorf("preview must not delete the file")
	}

	// ack=true deletes; content is snapshotted first so it's restorable.
	_, del, err := configDeleteTool(ctx, nil, configDeleteIn{Scope: "global", Ack: true})
	if err != nil {
		t.Fatalf("config_delete ack: %v", err)
	}
	if !del.Deleted {
		t.Errorf("ack=true should delete, got %+v", del)
	}
	if _, err := os.Stat(gp); !os.IsNotExist(err) {
		t.Errorf("file should be gone after ack delete")
	}

	// The pre-delete content is recoverable via config_history → restore.
	_, hist, err := configHistoryTool(ctx, nil, configHistoryIn{Scope: "global"})
	if err != nil {
		t.Fatalf("config_history global: %v", err)
	}
	if len(hist.Generations) == 0 {
		t.Fatalf("expected a snapshotted generation after delete")
	}
	if _, _, err := configRestoreTool(ctx, nil, configRestoreIn{Scope: "global", Generation: hist.Generations[0].Generation}); err != nil {
		t.Fatalf("config_restore global: %v", err)
	}
	if !strings.Contains(readFile(t, gp), "log_level: debug") {
		t.Errorf("restore did not bring back deleted content:\n%s", readFile(t, gp))
	}
}

// TestConfigLocate reports the global + repo files with correct
// existence flags.
func TestConfigLocate(t *testing.T) {
	ctx := context.Background()
	gp := globalCfgEnv(t)
	mustMkdir(t, filepath.Dir(gp))
	if err := os.WriteFile(gp, []byte("daemon:\n  log_level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := newTempRepo(t)

	_, out, err := configLocateTool(ctx, nil, configLocateIn{Repo: repo})
	if err != nil {
		t.Fatalf("config_locate: %v", err)
	}
	byScope := map[string]configFileInfo{}
	for _, f := range out.Files {
		byScope[f.Scope] = f
	}
	if g, ok := byScope["global"]; !ok || !g.Exists {
		t.Errorf("global file should exist in locate output: %+v", byScope)
	}
	if r, ok := byScope["repo"]; !ok || r.Exists {
		t.Errorf("repo .treeman.yaml should be listed as not-existing: %+v", byScope)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
