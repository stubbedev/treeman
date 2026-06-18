package daemon

import (
	"context"
	"path/filepath"
	"testing"
)

// TestReapDeadRepos verifies the boot reaper removes registry rows for
// a repo whose path is gone from disk while leaving a live repo (and
// its child rows) untouched.
func TestReapDeadRepos(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()

	// Live repo: its dir exists on disk.
	liveDir := t.TempDir()
	liveID, err := st.Store.EnsureRepo(ctx, liveDir, "live")
	if err != nil {
		t.Fatal(err)
	}
	liveWt, err := st.Store.EnsureWorktree(ctx, liveID, filepath.Join(liveDir, "w"), "w", "main")
	if err != nil {
		t.Fatal(err)
	}

	// Dead repo: a path that does not exist.
	deadID, err := st.Store.EnsureRepo(ctx, "/nonexistent/treeman-test-dead/main", "dead")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Store.EnsureWorktree(ctx, deadID, "/nonexistent/treeman-test-dead/main/w", "w", "main"); err != nil {
		t.Fatal(err)
	}

	ReapDeadRepos(ctx, st)

	refs, err := st.Store.ListRepoRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].ID != liveID {
		t.Fatalf("want only live repo %d to survive, got %+v", liveID, refs)
	}
	// Live worktree row must survive.
	if got, _ := st.Store.LoadWorktreePorts(ctx, liveWt); got == nil {
		t.Errorf("live worktree lookup broke after reap")
	}
	// Dead repo's worktree rows are gone (RemoveRepo cascade).
	var n int
	_ = st.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM worktrees WHERE repo_id = ?", deadID).Scan(&n)
	if n != 0 {
		t.Errorf("dead repo worktrees survived reap: %d", n)
	}
}
