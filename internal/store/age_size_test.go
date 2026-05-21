package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestListSnapshotsOlderThan seeds rows with mixed ages and
// asserts only those older than the cutoff come back, in
// last_used_at ASC order.
func TestListSnapshotsOlderThan(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UnixMilli()
	for i, age := range []int64{40, 31, 29, 5, 0} {
		err := st.RecordSnapshot(ctx, SnapshotRecord{
			Fingerprint:   "fp" + itoaTest(i),
			Engine:        "mysql",
			EngineVersion: "8.0",
			SourceDB:      "src",
			TemplateName:  "tpl" + itoaTest(i),
			LastUsedAt:    now - age*24*60*60*1000,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// 30-day cutoff.
	cutoff := now - 30*24*60*60*1000
	cands, err := st.ListSnapshotsOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("want 2 (ages 31 + 40), got %d", len(cands))
	}
	// Order ASC by last_used_at → oldest first → fp0 (40d) before fp1 (31d).
	if cands[0].Fingerprint != "fp0" || cands[1].Fingerprint != "fp1" {
		t.Errorf("unexpected order: %+v", cands)
	}
}

// TestListSnapshotsLargestLRU verifies the size-DESC,
// last_used_at-ASC ordering used by the size sweep.
func TestListSnapshotsLargestLRU(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	type row struct {
		fp   string
		size int64
		used int64
	}
	rows := []row{
		{"big-fresh", 100, 200},
		{"big-stale", 100, 100},
		{"small-fresh", 10, 300},
		{"tiny-stale", 1, 50},
	}
	for _, r := range rows {
		if err := st.RecordSnapshot(ctx, SnapshotRecord{
			Fingerprint: r.fp, Engine: "mysql", EngineVersion: "8.0",
			SourceDB: "src", TemplateName: "t-" + r.fp, LastUsedAt: r.used,
			SizeBytes: r.size,
		}); err != nil {
			t.Fatal(err)
		}
	}
	cands, sizes, err := st.ListSnapshotsLargestLRU(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"big-stale", "big-fresh", "small-fresh", "tiny-stale"}
	if len(cands) != len(want) {
		t.Fatalf("len=%d want %d", len(cands), len(want))
	}
	for i, w := range want {
		if cands[i].Fingerprint != w {
			t.Errorf("idx %d got %q want %q", i, cands[i].Fingerprint, w)
		}
	}
	// big-stale is first → size 100.
	if sizes[0] != 100 {
		t.Errorf("size[0]=%d want 100", sizes[0])
	}
}

func TestSumSnapshotBytes(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 3; i++ {
		_ = st.RecordSnapshot(ctx, SnapshotRecord{
			Fingerprint: "fp" + itoaTest(i), Engine: "mysql", EngineVersion: "8.0",
			SourceDB: "src", TemplateName: "t" + itoaTest(i), LastUsedAt: 1, SizeBytes: 50,
		})
	}
	sum, err := st.SumSnapshotBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum != 150 {
		t.Errorf("sum=%d want 150", sum)
	}
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for n := i; n > 0; n /= 10 {
		out = string(rune('0'+(n%10))) + out
	}
	return out
}
