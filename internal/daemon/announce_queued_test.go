package daemon

import (
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/pkg/rpc"
)

func TestDetachedFinalizeIsQueuedBeforeThePlanIsAcknowledged(t *testing.T) {
	st := newTestState(t)
	ctx := t.Context()
	repoID, err := st.Store.EnsureRepo(ctx, "/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	head, _ := st.Store.EnsureWorktree(ctx, repoID, "/repo/head", "head", "head")
	behind, _ := st.Store.EnsureWorktree(ctx, repoID, "/repo/behind", "behind", "behind")
	finalize := func(path string) rpc.Task {
		return rpc.Task{Type: rpc.TaskWorktreeFinalize, RepoPath: "/repo", WorktreePath: path}
	}
	groups := [][]rpc.Task{
		{finalize("/repo/head")},
		{{Type: rpc.TaskWorktreeRegister, RepoPath: "/repo"}, finalize("/repo/behind")},
		{finalize("/repo/unregistered")},
	}

	if err := announceQueuedFinalizes(runid.With(ctx, "run-224"), st, groups); err != nil {
		t.Fatal(err)
	}

	queued := func(wtID int64) []store.Event {
		evs, err := st.Store.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtWorktreeCreateQueued}})
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	got := queued(head)
	if len(got) != 1 {
		t.Fatalf("a finalize heading its lane must read queued on acknowledgement, got %+v", got)
	}
	if !strings.Contains(got[0].PayloadJSON, `"run_id":"run-224"`) {
		t.Errorf("queued event must carry the plan's run id, got %s", got[0].PayloadJSON)
	}
	if evs := queued(behind); len(evs) != 0 {
		t.Errorf("a finalize behind another task may never run and must not read queued, got %+v", evs)
	}
}

func TestFinalizeFailingBeforeItsIdentityClosesTheQueuedState(t *testing.T) {
	st, repo, wt := newWatchWorktreeWithConfig(t, "databases: [")
	ctx := t.Context()
	repoID, err := st.Store.EnsureRepo(ctx, repo, "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, wt, "feat", "feat")
	if err != nil {
		t.Fatal(err)
	}
	if err := announceQueuedFinalizes(
		ctx,
		st,
		[][]rpc.Task{{{Type: rpc.TaskWorktreeFinalize, RepoPath: repo, WorktreePath: wt}}},
	); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeWorktree(ctx, st, repo, wt, nil, false); err == nil {
		t.Fatal("an unparsable config must fail the finalize")
	}
	waitForWorktreeState(t, st, wtID, store.StateError)
}
