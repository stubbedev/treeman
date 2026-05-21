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
		`"repo"`, `"worktrees"`, `"connections"`, `"databases"`,
		`"hooks"`, `"snapshots"`, `"watcher"`, `"env_scoping"`,
	} {
		if !strings.Contains(body, key) {
			t.Errorf("schema missing top-level key %s", key)
		}
	}
}
