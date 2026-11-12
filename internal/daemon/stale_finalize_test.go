package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
)

func TestSweepStalePreparingEmitsErrorEvent(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()

	repoID, err := st.Store.EnsureRepo(ctx, "/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, "/repo/wt", "slug", "feature")
	if err != nil {
		t.Fatal(err)
	}
	// A finalize_start with no matching done — the "stuck in preparing"
	// state described by issue #9.
	if err := st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateStart,
		"prepare beginning", repoID, wtID, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	// Force the async batcher to drain so QueryEvents sees the row.

	SweepStalePreparing(ctx, st)

	evs, err := st.Store.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtWorktreeCreateError},
		Limit:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected one wt_finalize event, got %d", len(evs))
	}
	if evs[0].Level != store.LevelError {
		t.Errorf("level = %q, want error", evs[0].Level)
	}
}

// armedWt is the worktree path the watchdog tests arm a finalize for.
const armedWt = "/repo/wt"

// armedFinalize registers an in-flight finalize of armedWt whose lease
// last progressed at lastProgress and returns the cause it was stalled
// with, if any.
func armedFinalize(st *State, lastProgress time.Time, queued int) *error {
	var cause error
	st.inFlightMu.Lock()
	st.inFlightFinalizes[armedWt] = inFlightFinalize{
		cancel:    func() {},
		startedAt: lastProgress,
		lease: leaseState{
			stall:        func(err error) { cause = err },
			lastProgress: lastProgress,
			queuedClones: queued,
		},
	}
	st.inFlightMu.Unlock()
	return &cause
}

func createErrors(ctx context.Context, t *testing.T, st *State, wtID int64) []store.Event {
	t.Helper()
	st.Store.FlushEvents(ctx)
	evs, err := st.Store.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtWorktreeCreateError},
		Limit:      5,
	})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// A stalled finalize is cancelled with a stall cause and left to record
// its own failure; only one that is still in flight a watchdog interval
// later gets the error event from the watchdog.
func TestSweepExpiredFinalizesStallsThenReports(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()

	repoID, _ := st.Store.EnsureRepo(ctx, "/repo", "repo")
	wtID, _ := st.Store.EnsureWorktree(ctx, repoID, "/repo/wt", "slug", "feature")
	cause := armedFinalize(st, time.Now().Add(-2*finalizeTimeout), 0)

	sweepExpiredFinalizes(ctx, st)

	if _, ok := errors.AsType[*finalizeStalledError](*cause); !ok {
		t.Fatalf("expected the stalled finalize to be cancelled with a stall cause, got %v", *cause)
	}
	if evs := createErrors(ctx, t, st, wtID); len(evs) != 0 {
		t.Fatalf("the finalize records its own failure once cancelled; watchdog wrote %d events", len(evs))
	}

	st.updateLease("/repo/wt", func(ls *leaseState) { ls.stalledAt = time.Now().Add(-watchdogInterval) })
	sweepExpiredFinalizes(ctx, st)
	sweepExpiredFinalizes(ctx, st)

	evs := createErrors(ctx, t, st, wtID)
	if len(evs) != 1 {
		t.Fatalf("expected exactly 1 create:error for a finalize that ignored its cancel, got %d", len(evs))
	}
	if evs[0].Level != store.LevelError {
		t.Errorf("level = %q, want error", evs[0].Level)
	}
}

func TestSweepExpiredFinalizesSkipsFreshFinalize(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()

	repoID, _ := st.Store.EnsureRepo(ctx, "/repo", "repo")
	wtID, _ := st.Store.EnsureWorktree(ctx, repoID, "/repo/wt", "slug", "feature")
	cause := armedFinalize(st, time.Now(), 0)

	sweepExpiredFinalizes(ctx, st)

	if *cause != nil {
		t.Errorf("fresh in-flight finalize must not be cancelled, got %v", *cause)
	}
	if evs := createErrors(ctx, t, st, wtID); len(evs) != 0 {
		t.Errorf("expected no synthetic event for fresh finalize, got %d", len(evs))
	}
}

// Regression for #199: a prepare queued behind other worktrees' clone
// restores made no progress of its own for longer than the timeout and
// was cancelled while its clones were about to finish.
func TestSweepExpiredFinalizesSparesFinalizeWaitingForCloneSlot(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()
	cause := armedFinalize(st, time.Now().Add(-2*finalizeTimeout), 1)

	sweepExpiredFinalizes(ctx, st)

	if *cause != nil {
		t.Errorf("a finalize waiting for a clone slot must not be cancelled, got %v", *cause)
	}
}

// Regression for #199: the clock restarts when a queued restore is
// admitted and on every finished restore, so a finalize whose clones
// keep finishing never times out.
func TestCloneProgressRestartsTheClock(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()
	cause := armedFinalize(st, time.Now().Add(-2*finalizeTimeout), 0)
	progress := st.finalizeRunOptions("/repo/wt").Progress

	progress(prepare.CloneQueued)
	sweepExpiredFinalizes(ctx, st)
	progress(prepare.CloneDequeued)
	progress(prepare.CloneAdmitted)
	sweepExpiredFinalizes(ctx, st)

	if *cause != nil {
		t.Errorf("queued then admitted clones must keep the finalize alive, got %v", *cause)
	}
}

func TestLeaseVerdict(t *testing.T) {
	now := time.Now()
	stall := func(error) {}
	cases := []struct {
		name  string
		lease leaseState
		want  watchdogAction
	}{
		{"not armed", leaseState{lastProgress: now.Add(-2 * finalizeTimeout)}, watchdogNone},
		{"recent progress", leaseState{stall: stall, lastProgress: now}, watchdogNone},
		{"idle past the timeout", leaseState{stall: stall, lastProgress: now.Add(-finalizeTimeout)}, watchdogStall},
		{
			"idle but queued for a clone slot",
			leaseState{stall: stall, lastProgress: now.Add(-finalizeTimeout), queuedClones: 2},
			watchdogNone,
		},
		{"stalled just now", leaseState{stall: stall, stalledAt: now}, watchdogNone},
		{"stalled and still running", leaseState{stall: stall, stalledAt: now.Add(-watchdogInterval)}, watchdogReport},
		{"already reported", leaseState{stall: stall, stalledAt: now.Add(-watchdogInterval), reported: true}, watchdogNone},
	}
	for _, c := range cases {
		if got := c.lease.verdict(now); got != c.want {
			t.Errorf("%s: verdict = %v, want %v", c.name, got, c.want)
		}
	}
}

// Regression for #199: `clones:end ok=8 fail=0` landed after the timeout
// fired and the worktree stayed `error`. A prepare that succeeds despite
// a stall cancel renews the lease, so the finalize carries on to ready.
func TestAfterPrepareRenewsLeaseWhenStalledPrepareSucceeded(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	lease := st.leaseFinalize(context.Background(), "/repo/wt")
	defer lease.end()
	lease.stop(&finalizeStalledError{idle: finalizeTimeout})

	ctx, err := afterPrepare(lease, nil)
	if err != nil {
		t.Fatalf("a successful prepare must not fail: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the run must continue on a live context after a successful prepare")
	}
}

func TestAfterPrepareReportsStallWhenPrepareFailed(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	lease := st.leaseFinalize(context.Background(), "/repo/wt")
	defer lease.end()
	stalled := &finalizeStalledError{idle: finalizeTimeout}
	lease.stop(stalled)

	_, err := afterPrepare(lease, context.Canceled)
	if !errors.Is(err, stalled) {
		t.Fatalf("a prepare cut short by a stall must fail with the stall, got %v", err)
	}
}

func TestAfterPrepareTreatsPreemptAsCleanStop(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	parent, preempt := context.WithCancel(context.Background())
	lease := st.leaseFinalize(parent, "/repo/wt")
	defer lease.end()
	preempt()

	ctx, err := afterPrepare(lease, context.Canceled)
	if !preempted(ctx) {
		t.Fatal("the preempted run must see its preempt at the next boundary")
	}
	stop, stopErr := stopBefore(ctx, st, "post-hooks", 0, 0, err)
	if !stop || stopErr != nil {
		t.Fatalf("a teardown preempt is a clean stop, got stop=%v err=%v", stop, stopErr)
	}
}

func TestSweepStalePreparingSkipsCompleted(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()

	repoID, _ := st.Store.EnsureRepo(ctx, "/repo", "repo")
	wtID, _ := st.Store.EnsureWorktree(ctx, repoID, "/repo/wt", "slug", "feature")
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateStart,
		"prepare beginning", repoID, wtID, "", 0, nil)
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateEnd,
		"prepare complete", repoID, wtID, "", 0, nil)

	SweepStalePreparing(ctx, st)

	evs, _ := st.Store.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtWorktreeCreateError},
		Limit:      5,
	})
	if len(evs) != 0 {
		t.Errorf("expected no synthetic wt_finalize event for completed row, got %d", len(evs))
	}
}
