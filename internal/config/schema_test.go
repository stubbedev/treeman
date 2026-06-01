package config

import (
	"encoding/json"
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
