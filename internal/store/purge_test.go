package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestPruneOldLogsByCutoff covers the daemon-driven retention sweep
// (LogPruneLoop → Store.PruneOldLogs): rows older than the cutoff
// drop from events + hook_runs, and FK CASCADE wipes their
// hook_log_chunks. Newer rows survive untouched.
func TestPruneOldLogsByCutoff(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	repoID, _ := st.EnsureRepo(ctx, "/r", "r")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/r/w", "w", "main")

	oldTs := int64(1_000_000)
	newTs := int64(9_000_000)

	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO events(ts, level, event_type, payload_json) VALUES (?, 'info', 'old', '{}')`,
		oldTs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO events(ts, level, event_type, payload_json) VALUES (?, 'info', 'new', '{}')`,
		newTs); err != nil {
		t.Fatal(err)
	}

	oldHookID, err := st.WriteHookRun(ctx, wtID, "setup", 0, "old", oldTs, oldTs+5, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	newHookID, err := st.WriteHookRun(ctx, wtID, "setup", 0, "new", newTs, newTs+5, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendHookLogChunk(ctx, oldHookID, "merged", []byte("old body")); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendHookLogChunk(ctx, newHookID, "merged", []byte("new body")); err != nil {
		t.Fatal(err)
	}

	removed, err := st.PruneOldLogs(ctx, 5_000_000) // between old and new
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed=%d, want 2 (1 event + 1 hook_run)", removed)
	}

	// Hook log chunks for the deleted hook must cascade.
	oldChunks, _ := st.QueryHookLog(ctx, oldHookID)
	if len(oldChunks) != 0 {
		t.Errorf("old hook chunk survived FK CASCADE: %+v", oldChunks)
	}
	newChunks, _ := st.QueryHookLog(ctx, newHookID)
	if len(newChunks) != 1 || string(newChunks[0].Body) != "new body" {
		t.Errorf("new chunk lost or mangled: %+v", newChunks)
	}

	// keep_days <= 0 = no-op.
	if n, err := st.PruneOldLogs(ctx, 0); err != nil || n != 0 {
		t.Errorf("cutoff<=0 should be no-op, got n=%d err=%v", n, err)
	}
}

func TestPurgeEventsRequiresFilter(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

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
	defer func() { _ = st.Close() }()

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
	defer func() { _ = st.Close() }()

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
