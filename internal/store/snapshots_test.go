package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestListLRUEvictable verifies the LRU ordering + cap respects
// repo_id scoping. The same repo with 10 rows + cap=3 returns 7
// candidates in last_used_at ascending order.
func TestListLRUEvictable(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	repoID, err := st.EnsureRepo(ctx, "/tmp/myrepo", "myrepo")
	if err != nil {
		t.Fatal(err)
	}

	// Seed 10 snapshots with monotonically increasing LastUsedAt.
	for i := 0; i < 10; i++ {
		err := st.RecordSnapshot(ctx, SnapshotRecord{
			Fingerprint:   fingerprintN(i),
			Engine:        "mysql",
			EngineVersion: "8.0.0",
			SourceDB:      "myapp_src",
			TemplateName:  fingerprintTpl(i),
			LastUsedAt:    int64(1000 + i),
			RepoID:        repoID,
		})
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	// Cap = 3 → top-3 by LRU (the 3 most-recently-used) kept;
	// remaining 7 candidates returned for eviction, ordered by
	// last_used_at DESC inside the OFFSET (the slice is the tail of
	// `ORDER BY last_used_at DESC` after skipping the top 3).
	cands, err := st.ListLRUEvictable(ctx, repoID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cands); got != 7 {
		t.Fatalf("want 7 candidates, got %d", got)
	}
	// The freshest survivor was i=9; first eviction candidate is
	// i=6 (the row immediately past the cap when sorted DESC).
	wantFirst := fingerprintN(6)
	if cands[0].Fingerprint != wantFirst {
		t.Errorf("want first %q, got %q", wantFirst, cands[0].Fingerprint)
	}
}

func TestListLRUEvictableZeroCapNoop(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/myrepo", "myrepo")
	_ = st.RecordSnapshot(ctx, SnapshotRecord{
		Fingerprint: "f", Engine: "mysql", EngineVersion: "8.0", SourceDB: "src",
		TemplateName: "tpl", LastUsedAt: 1, RepoID: repoID,
	})
	cands, err := st.ListLRUEvictable(ctx, repoID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("cap=0 should return no candidates, got %d", len(cands))
	}
}

func fingerprintN(i int) string   { return "fp" + itoa(i) }
func fingerprintTpl(i int) string { return "tpl" + itoa(i) }
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for n := i; n > 0; n /= 10 {
		out = string(rune('0'+(n%10))) + out
	}
	return out
}
