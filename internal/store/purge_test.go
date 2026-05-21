package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPurgeEventsRequiresFilter(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Insert a couple of events so we can verify selective purge.
	for i := 0; i < 5; i++ {
		_ = st.WriteEvent(ctx, LevelInfo, "test", "msg", 0, 0, "", 0, nil)
	}
	for i := 0; i < 3; i++ {
		_ = st.WriteEvent(ctx, LevelError, "test", "boom", 0, 0, "", 0, nil)
	}

	// Purge only error-level rows.
	n, err := st.PurgeEvents(ctx, EventFilter{Levels: []string{"error"}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("removed %d, want 3", n)
	}
	remaining, _ := st.QueryEvents(ctx, EventFilter{Limit: 100})
	for _, e := range remaining {
		if e.Level == "error" {
			t.Errorf("error row survived: %+v", e)
		}
	}
}

func TestListSnapshotsForRepoZeroRepoReturnsNil(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := st.ListSnapshotsForRepo(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil for repoID=0", got)
	}
}

func TestListSnapshotsForRepoReturnsOnlyMatches(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	repoA, _ := st.EnsureRepo(ctx, "/a", "a")
	repoB, _ := st.EnsureRepo(ctx, "/b", "b")

	for _, r := range []SnapshotRecord{
		{Fingerprint: "fpA1", Engine: "mysql", TemplateName: "tA1", SourceDB: "src", RepoID: repoA, MigrationsHash: "h"},
		{Fingerprint: "fpA2", Engine: "mysql", TemplateName: "tA2", SourceDB: "src", RepoID: repoA, MigrationsHash: "h"},
		{Fingerprint: "fpB1", Engine: "mysql", TemplateName: "tB1", SourceDB: "src", RepoID: repoB, MigrationsHash: "h"},
	} {
		if err := st.RecordSnapshot(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.ListSnapshotsForRepo(ctx, repoA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Fingerprint == "fpB1" {
			t.Errorf("repoB snapshot leaked into repoA list")
		}
	}
}
