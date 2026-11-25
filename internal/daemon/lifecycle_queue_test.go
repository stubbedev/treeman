package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

func newQueuedFinalizeWorktree(t *testing.T) (st *State, repo, wt string, wtID int64) {
	t.Helper()
	st, repo, wt = newWatchWorktree(t, "'mkdir -p data && touch \"$TREEMAN_TARGET_DB\"'")
	st.lifecycleSlots = newLifecycleSlots(1)
	repoID, err := st.Store.EnsureRepo(t.Context(), repo, filepath.Base(repo))
	if err != nil {
		t.Fatal(err)
	}
	wtID, err = st.Store.EnsureWorktree(t.Context(), repoID, wt, "feat", "feat")
	if err != nil {
		t.Fatal(err)
	}
	return st, repo, wt, wtID
}

func waitForWorktreeState(t *testing.T, st *State, wtID int64, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		st.Store.FlushEvents(t.Context())
		last, err := st.Store.LatestWorktreeStateEvent(t.Context(), wtID)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ = store.DeriveWorktreeState(last); got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worktree #%d state = %q, want %q", wtID, got, want)
}

func TestFinalizeWaitsQueuedForALifecycleSlot(t *testing.T) {
	st, repo, wt, wtID := newQueuedFinalizeWorktree(t)
	hold, err := st.lifecycleSlots.acquire(t.Context(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- FinalizeWorktree(t.Context(), st, repo, wt, nil, false) }()

	waitForWorktreeState(t, st, wtID, store.StateQueued)
	select {
	case err := <-done:
		t.Fatalf("finalize ran while every lifecycle slot was busy (err %v)", err)
	default:
	}
	hold()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitForWorktreeState(t, st, wtID, store.StateReady)
}

func TestTeardownPreemptsAQueuedFinalize(t *testing.T) {
	st, repo, wt, wtID := newQueuedFinalizeWorktree(t)
	hold, err := st.lifecycleSlots.acquire(t.Context(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	done := make(chan error, 1)
	go func() { done <- FinalizeWorktree(t.Context(), st, repo, wt, nil, false) }()
	waitForWorktreeState(t, st, wtID, store.StateQueued)

	if !st.CancelFinalize(wt) {
		t.Fatal("a queued finalize must be registered in flight so a teardown can preempt it")
	}
	if err := <-done; err != nil {
		t.Fatalf("a finalize preempted while queued must stop cleanly, got %v", err)
	}
	if st.IsFinalizeInFlight(wt) {
		t.Error("a preempted queued finalize must release its in-flight claim")
	}
	evs, err := st.Store.QueryEvents(t.Context(), store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtWorktreeCreateStart},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Errorf("a finalize preempted while queued must not start, got %+v", evs)
	}
}
