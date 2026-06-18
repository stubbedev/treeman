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
	defer func() { _ = s.Close() }()

	repoID, _ := s.EnsureRepo(ctx, "/repos/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/repos/foo/.wt/x", "x", "feature/x")

	if _, err := s.WriteHookRun(ctx, wtID, "setup", 0,
		"composer install", 1000, 1250, 0, "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteHookRun(ctx, wtID, "setup", 1,
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

// TestPathLookupsAreCaseInsensitive verifies macOS APFS parity:
// a worktree stored with one casing must be findable under a
// different casing via every path-keyed lookup the daemon and CLI
// use to resolve cwd → row.
func TestPathLookupsAreCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	stored := "/Users/Jane/Repo"
	wt := stored + "/.worktrees/Feat"

	repoID, err := s.EnsureRepo(ctx, stored, "Repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := s.EnsureWorktree(ctx, repoID, wt, "feat", "feature/x")
	if err != nil {
		t.Fatal(err)
	}

	// LookupRepoID via mis-cased query.
	got, err := s.LookupRepoID(ctx, "/users/jane/repo")
	if err != nil || got != repoID {
		t.Errorf("LookupRepoID case-folded miss: got=%d err=%v want=%d", got, err, repoID)
	}

	// LookupActiveWorktreeByPath via mis-cased query.
	row, err := s.LookupActiveWorktreeByPath(ctx, "/users/jane/repo/.worktrees/feat")
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != wtID {
		t.Errorf("LookupActiveWorktreeByPath case-folded miss: got=%d want=%d", row.ID, wtID)
	}

	// EnsureWorktree on a mis-cased path must resurrect the SAME row,
	// not insert a duplicate.
	dupID, err := s.EnsureWorktree(ctx, repoID, "/users/jane/repo/.worktrees/FEAT", "feat", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if dupID != wtID {
		t.Errorf("EnsureWorktree inserted duplicate row under different case: got=%d want=%d", dupID, wtID)
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
	defer func() { _ = s.Close() }()
	if id, err := s.WriteHookRun(ctx, 0, "setup", 0, "noop", 1, 2, 0, "", ""); err != nil || id != 0 {
		t.Fatalf("zero-wt should be a silent no-op, got id=%d err=%v", id, err)
	}
}
