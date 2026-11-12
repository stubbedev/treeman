package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// explain returns the EXPLAIN QUERY PLAN detail lines for q.
func explain(t *testing.T, st *Store, q string, args ...any) string {
	t.Helper()
	rows, err := st.DB.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func assertServedWithoutSort(t *testing.T, st *Store, q string, args []any, index string) {
	t.Helper()
	plan := explain(t, st, q, args...)
	if !strings.Contains(plan, index) || strings.Contains(plan, "SCAN events") || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("%s\nplan:\n%s\nwant it served by %s with no scan or sort", q, plan, index)
	}
}

func TestRepoScopedEventQueriesUseAnIndex(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	repoPage, repoArgs := queryEventsSQL(EventFilter{RepoID: 1, Limit: 50})
	assertServedWithoutSort(t, st, repoPage, repoArgs, "idx_events_repo_recent")
	assertServedWithoutSort(t, st, "DELETE FROM events WHERE repo_id = ?", []any{1}, "idx_events_repo_recent")
	assertServedWithoutSort(t, st, "DELETE FROM events WHERE worktree_id IN (SELECT id FROM worktrees WHERE repo_id = ?)", []any{1},
		"idx_events_worktree_recent")
}

// Two processes opening a fresh database right after an upgrade used
// to both see a migration as pending; the loser failed with
// "duplicate column" / "table already exists".
func TestConcurrentOpensApplyMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Go(func() {
			st, err := Open(context.Background(), path)
			if err != nil {
				errs <- err
				return
			}
			errs <- st.Close()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent open: %v", err)
		}
	}
}

func TestWorktreeLifecycleEventQueriesUseTheWorktreeIndexWithoutSorting(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	page, pageArgs := queryEventsSQL(EventFilter{WorktreeID: 1, EventTypes: WorktreeStateEventTypes, Limit: 1})
	assertServedWithoutSort(t, st, page, pageArgs, "idx_events_worktree_recent")

	latest, latestArgs := latestEventPerWorktreeSQL([]int64{1, 2, 3}, WorktreeStateEventTypes)
	assertServedWithoutSort(t, st, latest, latestArgs, "COVERING INDEX idx_events_worktree_recent")
}

func TestActiveWorktreePageUsesTheCoveringPartialIndexWithoutSorting(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	plan := explain(t, st, activeWorktreePageSQL, int64(0), 201)
	if !strings.Contains(plan, "COVERING INDEX idx_worktrees_active_page") || strings.Contains(plan, "SCAN") ||
		strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("plan:\n%s\nwant a covering idx_worktrees_active_page seek with no scan or sort", plan)
	}
}

func TestOpenWaitsForAWriterBeforeSwitchingToWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE held (x)"); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = conn.ExecContext(ctx, "COMMIT")
		_ = conn.Close()
		_ = raw.Close()
		close(released)
	}()
	st, err := Open(ctx, path)
	<-released
	if err != nil {
		t.Fatalf("Open while another connection held a write lock: %v", err)
	}
	defer func() { _ = st.Close() }()
	var mode string
	if err := st.DB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode = %q, %v; want wal", mode, err)
	}
}

// PendingCreateRetries runs on every watchdog tick: both of its queries
// must be index-served, with no scan of the events table.
func TestPendingCreateRetriesQueriesUseAnIndex(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	latest, latestArgs := latestEventPerWorktreeSQL([]int64{1, 2, 3}, WorktreeOpEventTypes)
	assertServedWithoutSort(t, st, latest, latestArgs, "COVERING INDEX idx_events_worktree_recent")

	plan := explain(t, st, activeWorktreeRowsQuery)
	for line := range strings.SplitSeq(plan, "\n") {
		if strings.HasPrefix(line, "SCAN e") {
			t.Errorf("plan:\n%s\nwant the teardown check served by an events index, not a scan", plan)
		}
	}
	if !strings.Contains(plan, "idx_events_worktree") {
		t.Errorf("plan:\n%s\nwant the teardown check served by idx_events_worktree*", plan)
	}
}
