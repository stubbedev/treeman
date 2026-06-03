package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestFreshDBHasExpectedSchema opens a brand-new store DB and asserts
// the consolidated init migration produced the right set of tables
// + indexes. Acts as a regression net against future drift between
// 0001_init.sql and the schema the store code expects to query.
func TestFreshDBHasExpectedSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tm.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()

	got := dump(t, raw)
	wantTables := []string{
		"_treeman_migrations",
		"active_branch_db",
		"branch_db_migrated",
		"branch_durables",
		"config_generations",
		"dir_hashes",
		"events",
		"file_hashes",
		"hook_log_chunks",
		"hook_runs",
		"repos",
		"snapshots",
		"worktree_ports",
		"worktrees",
	}
	if !equalLists(got.tables, wantTables) {
		t.Errorf("tables\n got: %v\nwant: %v", got.tables, wantTables)
	}
	// Indexes from 0001_init.sql. Auto-generated sqlite_autoindex_* are
	// filtered out — only user-declared CREATE INDEXes show up here.
	wantIndexes := []string{
		"idx_active_branch_db_repo",
		"idx_branch_durables_branch",
		"idx_branch_durables_repo",
		"idx_config_generations_repo",
		"idx_events_ts",
		"idx_events_type",
		"idx_events_worktree",
		"idx_file_hashes_content",
		"idx_hook_log_chunks_run",
		"idx_hook_log_chunks_ts",
		"idx_hook_runs_worktree",
		"idx_repos_path_nocase",
		"idx_snapshots_lru",
		"idx_snapshots_migrations",
		"idx_snapshots_repo_lru",
		"idx_worktree_ports_one_per_port",
		"idx_worktree_ports_one_per_slot",
		"idx_worktree_ports_repo",
		"idx_worktrees_admin_dir",
		"idx_worktrees_one_main_per_repo",
		"idx_worktrees_path_nocase",
		"idx_worktrees_repo",
		"idx_worktrees_repo_visited",
		"idx_worktrees_slug",
	}
	if !equalLists(got.indexes, wantIndexes) {
		t.Errorf("indexes\n got: %v\nwant: %v", got.indexes, wantIndexes)
	}

	// Sanity: the binlog_checkpoints table is gone (was 0003 in the
	// old chain; squashed out when the binlog feature was removed).
	for _, tbl := range got.tables {
		if tbl == "binlog_checkpoints" {
			t.Errorf("binlog_checkpoints should not exist in fresh DBs")
		}
	}
}

type schemaSnapshot struct {
	tables  []string
	indexes []string
}

func dump(t *testing.T, db *sql.DB) schemaSnapshot {
	t.Helper()
	rows, err := db.Query(`
		SELECT type, name FROM sqlite_master
		WHERE type IN ('table','index')
		  AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var s schemaSnapshot
	for rows.Next() {
		var typ, name string
		_ = rows.Scan(&typ, &name)
		switch typ {
		case "table":
			s.tables = append(s.tables, name)
		case "index":
			s.indexes = append(s.indexes, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(s.tables)
	sort.Strings(s.indexes)
	return s
}

func equalLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}
