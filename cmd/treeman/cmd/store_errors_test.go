package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

func newBrokenStoreFixture(t *testing.T) (st *store.Store, wtID int64) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "treeman.db")
	t.Setenv("TREEMAN_DB_PATH", dbPath)
	t.Chdir(t.TempDir())
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repoID, err := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err = st.EnsureWorktree(ctx, repoID, "/tmp/repo/wt", "feat", "feat")
	if err != nil {
		t.Fatal(err)
	}
	return st, wtID
}

func dropTable(t *testing.T, st *store.Store, table string) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(), "DROP TABLE "+table); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeShowFailsWhenAReadFails(t *testing.T) {
	for _, tc := range []struct{ table, want string }{
		{"hook_runs", "hook runs"},
		{"worktree_ports", "ports"},
		{"active_branch_db", "branch_scoped"},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(tc.table, func(t *testing.T) {
				st, _ := newBrokenStoreFixture(t)
				dropTable(t, st, tc.table)
				err := showWorktree(context.Background(), showOpts{events: 5, hooks: 5, asJSON: asJSON, noPager: true}, "feat")
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "worktree #") {
					t.Fatalf(
						"show (json=%v) with %s unreadable: want an error naming %q and the worktree, got %v",
						asJSON,
						tc.table,
						tc.want,
						err,
					)
				}
			})
		}
	}
}

func TestPrepareSummaryFailsWhenEventsAreUnreadable(t *testing.T) {
	st, wtID := newBrokenStoreFixture(t)
	dropTable(t, st, "events")
	if err := printWtPrepareSummary(context.Background(), st, wtID); err == nil {
		t.Fatal("an unreadable event log must fail the prepare summary, not print nothing")
	}
	if _, err := finalizeAnchor(context.Background(), st, wtID); err == nil {
		t.Fatal("an unreadable event log must fail `worktree wait`'s anchor lookup, not anchor at creation time")
	}
}

func TestExplicitUnknownRepoFailsInsteadOfWideningTheScope(t *testing.T) {
	st, _ := newBrokenStoreFixture(t)
	if _, err := resolveShowRepoID(context.Background(), st, "/nowhere/unregistered"); err == nil ||
		!strings.Contains(err.Error(), "/nowhere/unregistered") || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("an unregistered --repo must fail naming the path, got %v", err)
	}
	if id, err := resolveShowRepoID(context.Background(), st, ""); err != nil || id != 0 {
		t.Fatalf("a cwd outside any repo stays unscoped: got %d, %v", id, err)
	}
}

func TestLookupWorktreeContainingCwdSurfacesQueryErrors(t *testing.T) {
	st, _ := newBrokenStoreFixture(t)
	row, err := lookupWorktreeContainingCwd(context.Background(), st, "/tmp/repo/wt/sub")
	if err != nil || row.Path != "/tmp/repo/wt" {
		t.Fatalf("nested cwd must resolve to its worktree: %+v, %v", row, err)
	}
	dropTable(t, st, "worktrees")
	if _, err := lookupWorktreeContainingCwd(context.Background(), st, "/tmp/repo/wt/sub"); err == nil {
		t.Fatal("an unreadable worktrees table must fail the cwd lookup, not read as 'not in a worktree'")
	}
}

func TestPatchTemplateContextSurfacesStoreErrors(t *testing.T) {
	st, _ := newBrokenStoreFixture(t)
	dropTable(t, st, "worktree_ports")
	if _, err := buildTemplateContext(context.Background(), "/tmp/repo", "/tmp/repo/wt"); err == nil ||
		!strings.Contains(err.Error(), "/tmp/repo/wt") {
		t.Fatalf("an unreadable ports table must surface naming the worktree, got %v", err)
	}
	if _, err := buildTemplateContext(context.Background(), "/tmp/other", "/tmp/other/wt"); err != nil {
		t.Fatalf("an unregistered repo still renders from the path slug: %v", err)
	}
}

func TestPickerSourcesSurfaceStoreErrors(t *testing.T) {
	st, _ := newBrokenStoreFixture(t)
	dropTable(t, st, "worktrees")
	if _, err := branchOccupancy(context.Background(), "/tmp/repo"); err == nil {
		t.Error("branchOccupancy must fail on an unreadable registry")
	}
	if _, err := tearingDownPaths(context.Background(), "/tmp/repo"); err == nil {
		t.Error("tearingDownPaths must fail on an unreadable registry")
	}
	if _, _, err := registryWorktreeForBranch(context.Background(), "/tmp/repo", "feat"); err == nil {
		t.Error("registryWorktreeForBranch must fail on an unreadable registry")
	}
	if _, err := liveWorktreePaths(context.Background(), "/tmp/repo"); err == nil {
		t.Error("liveWorktreePaths must fail on an unreadable registry")
	}
	if _, err := registrySlug(context.Background(), "/tmp/repo", "/tmp/repo/wt"); err == nil {
		t.Error("registrySlug must fail on an unreadable registry")
	}
}

func TestDoctorSnapshotTotalsSurfaceStoreErrors(t *testing.T) {
	st, _ := newBrokenStoreFixture(t)
	dropTable(t, st, "snapshots")
	if _, _, err := snapshotTotals(context.Background(), st, "/tmp/repo"); err == nil {
		t.Fatal("doctor must report an unreadable snapshot table, not a zero total")
	}
}
