package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// A snapshot recorded with no input vectors (the dump-only template) must
// still read back with non-nil maps: the MCP output schemas are reflected
// off SnapshotRecord and declare both as objects, so a nil map marshals to
// `null` and fails the tool's own output validation (#25).
func TestSnapshotMapsNeverMarshalNull(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.RecordSnapshot(ctx, SnapshotRecord{
		Fingerprint:   "fp-dumponly",
		Engine:        "mysql",
		EngineVersion: "8.0",
		SourceDB:      "src",
		TemplateName:  "tpl",
		Inputs:        nil,
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		get  func() (*SnapshotRecord, error)
	}{
		{"LookupSnapshot", func() (*SnapshotRecord, error) { return st.LookupSnapshot(ctx, "fp-dumponly") }},
		{"LookupAndTouchSnapshot", func() (*SnapshotRecord, error) { return st.LookupAndTouchSnapshot(ctx, "fp-dumponly") }},
	} {
		rec, err := tc.get()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rec == nil {
			t.Fatalf("%s: no row", tc.name)
		}
		if rec.Inputs == nil || rec.LockfileHashes == nil {
			t.Fatalf("%s: nil map: inputs=%v lockfiles=%v", tc.name, rec.Inputs, rec.LockfileHashes)
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.Contains(string(b), "null") {
			t.Fatalf("%s: marshalled null: %s", tc.name, b)
		}
	}
}
