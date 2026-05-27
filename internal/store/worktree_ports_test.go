package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStoreWithWt(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := Open(ctx, filepath.Join(dir, "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	repoID, err := st.EnsureRepo(ctx, "/repos/test", "test")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, "/repos/test/.worktrees/feature-x", "feature_x", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	return st, repoID, wtID
}

func TestAllocateAndLoadPorts(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)

	if err := st.AllocateWorktreePort(ctx, repoID, wtID, "octane", 8042); err != nil {
		t.Fatal(err)
	}
	if err := st.AllocateWorktreePort(ctx, repoID, wtID, "webpack", 3042); err != nil {
		t.Fatal(err)
	}
	ports, err := st.LoadWorktreePorts(ctx, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if ports["octane"] != 8042 || ports["webpack"] != 3042 {
		t.Errorf("unexpected map: %v", ports)
	}
}

func TestAllocateReturnsErrPortInUse(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)
	wt2, err := st.EnsureWorktree(ctx, repoID, "/repos/test/.worktrees/feature-y", "feature_y", "feature/y")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AllocateWorktreePort(ctx, repoID, wtID, "octane", 8042); err != nil {
		t.Fatal(err)
	}
	err = st.AllocateWorktreePort(ctx, repoID, wt2, "octane", 8042)
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("want ErrPortInUse, got %v", err)
	}
}

func TestAllocateRejectsDoubleSameSlot(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)
	if err := st.AllocateWorktreePort(ctx, repoID, wtID, "octane", 8042); err != nil {
		t.Fatal(err)
	}
	if err := st.AllocateWorktreePort(ctx, repoID, wtID, "octane", 8043); err == nil {
		t.Errorf("want error for double-allocating same slot on same worktree")
	}
}

func TestListUsedPortsExcludesDeletedWorktree(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)
	if err := st.AllocateWorktreePort(ctx, repoID, wtID, "octane", 8042); err != nil {
		t.Fatal(err)
	}
	used, err := st.ListUsedPorts(ctx, repoID, "octane")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := used[8042]; !ok {
		t.Fatalf("expected 8042 in used set, got %v", used)
	}
	if err := st.MarkWorktreeDeleted(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	used, err = st.ListUsedPorts(ctx, repoID, "octane")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := used[8042]; ok {
		t.Errorf("deleted worktree's port should not appear in used set: %v", used)
	}
}

func TestReleaseWorktreePorts(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openTestStoreWithWt(t)
	if err := st.AllocateWorktreePort(ctx, repoID, wtID, "octane", 8042); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseWorktreePorts(ctx, wtID); err != nil {
		t.Fatal(err)
	}
	ports, err := st.LoadWorktreePorts(ctx, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 0 {
		t.Errorf("expected empty map after release, got %v", ports)
	}
}
