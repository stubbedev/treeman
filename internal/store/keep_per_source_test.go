package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestListSnapshotsBeyondPerSource seeds two sources (migrations_hash
// groups) and asserts only the rows beyond `keep` per source — the LRU
// ones — come back, leaving the `keep` most-recently-used per source.
func TestListSnapshotsBeyondPerSource(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// Source A: 4 templates, last_used 10/20/30/40 (40 = most recent).
	for i, used := range []int64{10, 20, 30, 40} {
		mustRecord(t, st, "a"+itoaTest(i), "migA", used)
	}
	// Source B: a single template — never over any keep≥1 limit.
	mustRecord(t, st, "b0", "migB", 5)

	// keep=2 → per source keep the 2 newest, evict the rest. Source A
	// loses its two oldest (used 10, 20); source B keeps its one.
	cands, err := st.ListSnapshotsBeyondPerSource(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("keep=2: want 2 evictable (source A's 2 oldest), got %d: %+v", len(cands), cands)
	}
	// Ordered last_used_at ASC → oldest first: a0 (used 10) then a1 (20).
	if cands[0].Fingerprint != "a0" || cands[1].Fingerprint != "a1" {
		t.Errorf("unexpected eviction set/order: %+v (want a0 then a1)", cands)
	}

	// keep=0 is the disabled guard: must never return candidates (a 0
	// must not wipe every cached template).
	if got, err := st.ListSnapshotsBeyondPerSource(ctx, 0); err != nil || len(got) != 0 {
		t.Errorf("keep=0 must return no candidates, got %d (err=%v)", len(got), err)
	}

	// keep larger than every source → nothing evictable.
	if got, err := st.ListSnapshotsBeyondPerSource(ctx, 10); err != nil || len(got) != 0 {
		t.Errorf("keep=10 must return no candidates, got %d (err=%v)", len(got), err)
	}
}

func mustRecord(t *testing.T, st *Store, fp, migHash string, used int64) {
	t.Helper()
	if err := st.RecordSnapshot(context.Background(), SnapshotRecord{
		Fingerprint:    fp,
		Engine:         "mysql",
		EngineVersion:  "8.0",
		SourceDB:       "src_" + fp,
		TemplateName:   "tpl_" + fp,
		MigrationsHash: migHash,
		LastUsedAt:     used,
	}); err != nil {
		t.Fatalf("record %s: %v", fp, err)
	}
}
