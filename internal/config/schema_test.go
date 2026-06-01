package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/invopop/jsonschema"
)

// TestSchemaCoversKnownYAMLKeys asserts that the reflector with
// FieldNameTag="yaml" emits the documented top-level keys so that
// editor YAML completion isn't silently broken when a Config field
// drifts.
func TestSchemaCoversKnownYAMLKeys(t *testing.T) {
	r := &jsonschema.Reflector{
		Anonymous:      true,
		ExpandedStruct: true,
		FieldNameTag:   "yaml",
	}
	s := r.Reflect(&Config{})
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, key := range []string{
		`"worktrees"`, `"connections"`, `"databases"`,
		`"hooks"`, `"snapshots"`, `"debounce_ms"`, `"env_sources"`,
	} {
		if !strings.Contains(body, key) {
			t.Errorf("schema missing top-level key %s", key)
		}
	}
}

// TestFieldScopesCoversEveryTopLevelKey asserts every top-level Config
// key carries a known scope tag (global/repo/both) — a new field added
// without a `scope:` tag defaults to "both", which this test documents
// and which keeps it visible in every scoped schema rather than silently
// dropped.
func TestFieldScopesCoversEveryTopLevelKey(t *testing.T) {
	scopes := FieldScopes()
	want := map[string]string{
		"daemon":        "global",
		"snapshots":     "global",
		"logs":          "global",
		"status":        "global",
		"notifications": "global",
		"env_sources":   "repo",
		"patches":       "repo",
		"databases":     "repo",
		"hooks":         "repo",
		"main_worktree": "repo",
		"connections":   "both",
		"worktrees":     "both",
		"debounce_ms":   "both",
		"frameworks":    "both",
		"auto_fetch":    "both",
		"ports":         "both",
	}
	if len(scopes) != len(want) {
		t.Errorf("FieldScopes has %d keys, want %d: %v", len(scopes), len(want), scopes)
	}
	for k, v := range want {
		if scopes[k] != v {
			t.Errorf("scope[%q] = %q, want %q", k, scopes[k], v)
		}
	}
	for k, v := range scopes {
		switch v {
		case "global", "repo", "both":
		default:
			t.Errorf("key %q has invalid scope %q", k, v)
		}
	}
}

// TestCheckLayerScopeRejectsMisplacedKeys is the hard-break guard: a
// global-only key in a repo file (and vice versa) must error; "both"
// keys and correctly-placed keys pass.
func TestCheckLayerScopeRejectsMisplacedKeys(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// global-only key in a repo file → error.
	repoBad := write("repo_bad.yaml", "daemon:\n  log_level: debug\nworktrees:\n  root: .worktrees\n")
	if err := checkLayerScope(repoBad, "repo"); err == nil {
		t.Errorf("repo file with daemon: should be rejected")
	} else if !strings.Contains(err.Error(), "daemon") {
		t.Errorf("error should name the offending key: %v", err)
	}

	// repo-only key in the global file → error.
	globalBad := write("global_bad.yaml", "databases:\n  - engine: mysql\n")
	if err := checkLayerScope(globalBad, "global"); err == nil {
		t.Errorf("global file with databases: should be rejected")
	} else if !strings.Contains(err.Error(), "databases") {
		t.Errorf("error should name the offending key: %v", err)
	}

	// Correctly-placed + "both" keys pass in both layers.
	repoOK := write("repo_ok.yaml", "databases:\n  - engine: mysql\nconnections: {}\nworktrees:\n  root: .worktrees\n")
	if err := checkLayerScope(repoOK, "repo"); err != nil {
		t.Errorf("valid repo file rejected: %v", err)
	}
	globalOK := write("global_ok.yaml", "daemon:\n  log_level: info\nauto_fetch:\n  enabled: true\n")
	if err := checkLayerScope(globalOK, "global"); err != nil {
		t.Errorf("valid global file rejected: %v", err)
	}

	// Missing file is not an error.
	if err := checkLayerScope(filepath.Join(dir, "nope.yaml"), "repo"); err != nil {
		t.Errorf("missing file should pass: %v", err)
	}
}

// TestLoadLayeredRejectsGlobalKeyInRepoFile is the end-to-end hard break
// through the real loader.
func TestLoadLayeredRejectsGlobalKeyInRepoFile(t *testing.T) {
	// Isolate the global config so a real ~/.config doesn't interfere.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	body := "snapshots:\n  max_age_days: 5\nworktrees:\n  root: .worktrees\n"
	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLayered(repo); err == nil {
		t.Errorf("LoadLayered should reject a repo file with global-only snapshots:")
	} else if !strings.Contains(err.Error(), "snapshots") {
		t.Errorf("error should name snapshots: %v", err)
	}
}
