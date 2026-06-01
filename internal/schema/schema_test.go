package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderProducesObjectSchema(t *testing.T) {
	b, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("rendered schema is not valid JSON: %v", err)
	}
	if s["$schema"] == nil {
		t.Errorf("missing $schema declaration")
	}
}

// scopedKeys renders the scoped schema and returns its top-level
// property names as a set.
func scopedKeys(t *testing.T, scope Scope) map[string]bool {
	t.Helper()
	b, err := RenderScoped(scope)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("scoped schema not valid JSON: %v", err)
	}
	out := make(map[string]bool, len(s.Properties))
	for k := range s.Properties {
		out[k] = true
	}
	return out
}

func TestReflectScopedGlobalDropsRepoOnlyKeys(t *testing.T) {
	g := scopedKeys(t, ScopeGlobal)
	// Global-only + both keys present.
	for _, k := range []string{"daemon", "snapshots", "logs", "status", "notifications", "connections", "auto_fetch"} {
		if !g[k] {
			t.Errorf("global schema missing expected key %q", k)
		}
	}
	// Repo-only keys dropped.
	for _, k := range []string{"databases", "patches", "hooks", "main_worktree", "env_sources"} {
		if g[k] {
			t.Errorf("global schema should not contain repo-only key %q", k)
		}
	}
}

func TestReflectScopedRepoDropsGlobalOnlyKeys(t *testing.T) {
	r := scopedKeys(t, ScopeRepo)
	// Repo-only + both keys present.
	for _, k := range []string{"databases", "patches", "hooks", "main_worktree", "env_sources", "connections", "worktrees"} {
		if !r[k] {
			t.Errorf("repo schema missing expected key %q", k)
		}
	}
	// Global-only keys dropped.
	for _, k := range []string{"daemon", "snapshots", "logs", "status", "notifications"} {
		if r[k] {
			t.Errorf("repo schema should not contain global-only key %q", k)
		}
	}
}

func TestReflectScopedFullEqualsReflect(t *testing.T) {
	full := scopedKeys(t, ScopeFull)
	if len(full) != 16 {
		t.Errorf("full scope = %d top-level keys, want 16", len(full))
	}
}

func TestInstallGlobalWritesScopedSchemaAndModeline(t *testing.T) {
	// Point the global config dir at a temp location so the install
	// doesn't touch the real ~/.config.
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	// Seed a global config so the modeline has a file to land in.
	globalCfg := filepath.Join(cfgDir, "treeman", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(globalCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalCfg, []byte("daemon:\n  log_level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, _, err := Install("", TargetGlobal)
	if err != nil {
		t.Fatal(err)
	}
	// Installed schema is global-scoped (no repo-only keys).
	b, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read installed schema: %v", err)
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Properties["databases"]; ok {
		t.Errorf("global-installed schema must not contain repo-only 'databases'")
	}
	if _, ok := s.Properties["daemon"]; !ok {
		t.Errorf("global-installed schema missing 'daemon'")
	}
	// Modeline landed in the GLOBAL config, not a repo file.
	out, _ := os.ReadFile(globalCfg)
	if !strings.Contains(string(out), "$schema="+resolved) {
		t.Errorf("global config modeline not pointed at installed schema:\n%s", out)
	}
}

func TestSetModelineInsertsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, ".treeman.yaml")
	body := "repo:\n  name: foo\n"
	if err := os.WriteFile(repo, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := SetModeline(dir, "/tmp/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("changed=false on insert")
	}
	out, _ := os.ReadFile(repo)
	if !strings.HasPrefix(string(out), "# yaml-language-server: $schema=/tmp/schema.json\n") {
		t.Errorf("modeline not prepended:\n%s", out)
	}
}

func TestSetModelineReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, ".treeman.yaml")
	body := "# yaml-language-server: $schema=old\nrepo:\n  name: foo\n"
	if err := os.WriteFile(repo, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := SetModeline(dir, "new")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("changed=false on replace")
	}
	out, _ := os.ReadFile(repo)
	if !strings.Contains(string(out), "$schema=new") {
		t.Errorf("modeline not replaced:\n%s", out)
	}
	if strings.Contains(string(out), "$schema=old") {
		t.Errorf("old modeline left behind:\n%s", out)
	}
}

func TestSetModelineNoChangeWhenIdentical(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, ".treeman.yaml")
	body := "# yaml-language-server: $schema=same\nrepo:\n  name: foo\n"
	if err := os.WriteFile(repo, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := SetModeline(dir, "same")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("changed=true when modeline already matches")
	}
}

func TestSetModelineMissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	changed, err := SetModeline(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("changed=true with no .treeman.yaml")
	}
}

func TestReadModeline(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, ".treeman.yaml")
	_ = os.WriteFile(repo, []byte("# yaml-language-server: $schema=https://example.com/s.json\n"), 0o644)
	got := ReadModeline(dir)
	if got != "https://example.com/s.json" {
		t.Errorf("ReadModeline = %q, want https://example.com/s.json", got)
	}
}

func TestInstallTargetURLSkipsFileWrite(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".treeman.yaml"), []byte("repo: {}\n"), 0o644)
	resolved, _, err := Install(dir, TargetURL)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != URL {
		t.Errorf("resolved = %q, want %q", resolved, URL)
	}
	// File-write path should be empty for URL target.
	if _, err := os.Stat(filepath.Join(dir, "schemas")); err == nil {
		t.Errorf("schemas dir created for TargetURL")
	}
}

func TestInstallTargetRepoWritesFile(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".treeman.yaml"), []byte("repo: {}\n"), 0o644)
	resolved, _, err := Install(dir, TargetRepo)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "schemas", "treeman.schema.json")
	if resolved != want {
		t.Errorf("resolved = %q, want %q", resolved, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("schema file not written: %v", err)
	}
}
