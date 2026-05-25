package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestWriteHookRunPersistsAndQueries covers the basic round-trip: a
// PersistOutcome-shaped insert lands in hook_runs, and QueryHookRuns
// reads it back with the new command / group_idx columns wired
// through the 0008 migration.
func TestWriteHookRunPersistsAndQueries(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	repoID, _ := s.EnsureRepo(ctx, "/repos/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/repos/foo/.wt/x", "x", "feature/x")

	if err := s.WriteHookRun(ctx, wtID, "setup", 0,
		"composer install", 1000, 1250, 0, "ok", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteHookRun(ctx, wtID, "setup", 1,
		"yarn install", 1000, 1400, 2, "", "boom\n"); err != nil {
		t.Fatal(err)
	}

	runs, err := s.QueryHookRuns(ctx, wtID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d rows want 2", len(runs))
	}
	// Newest-first ordering — but identical started_at, so check by
	// group_idx instead of relying on insert order.
	var ok, fail HookRun
	for _, r := range runs {
		switch r.GroupIdx {
		case 0:
			ok = r
		case 1:
			fail = r
		}
	}
	if ok.Command != "composer install" {
		t.Errorf("command not persisted: %q", ok.Command)
	}
	if !ok.ExitCode.Valid || ok.ExitCode.Int64 != 0 {
		t.Errorf("ok run exit_code = %+v", ok.ExitCode)
	}
	if !fail.ExitCode.Valid || fail.ExitCode.Int64 != 2 {
		t.Errorf("fail run exit_code = %+v", fail.ExitCode)
	}
	if fail.StderrTail != "boom\n" {
		t.Errorf("stderr tail not persisted: %q", fail.StderrTail)
	}
}

// TestWriteHookRunZeroWorktreeIsNoop guards the "store wired but no
// row resolved" case PersistOutcome short-circuits on.
func TestWriteHookRunZeroWorktreeIsNoop(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.WriteHookRun(ctx, 0, "setup", 0, "noop", 1, 2, 0, "", ""); err != nil {
		t.Fatalf("zero-wt should be a silent no-op, got %v", err)
	}
}
