package daemon

import (
	"context"
	"testing"
	"time"

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

func TestSweepExpiredFinalizesCancelsAndEmitsError(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()

	repoID, _ := st.Store.EnsureRepo(ctx, "/repo", "repo")
	wtID, _ := st.Store.EnsureWorktree(ctx, repoID, "/repo/wt", "slug", "feature")

	// Simulate an in-flight finalize that started before the cutoff.
	cancelled := false
	st.inFlightMu.Lock()
	st.inFlightFinalizes["/repo/wt"] = inFlightFinalize{
		cancel:    func() { cancelled = true },
		startedAt: time.Now().Add(-2 * finalizeTimeout),
	}
	st.inFlightMu.Unlock()

	sweepExpiredFinalizes(ctx, st)

	if !cancelled {
		t.Errorf("expected expired in-flight finalize to be cancelled")
	}
	evs, _ := st.Store.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtWorktreeCreateError},
		Limit:      1,
	})
	if len(evs) != 1 {
		t.Fatalf("expected 1 wt_finalize event, got %d", len(evs))
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

	cancelled := false
	st.inFlightMu.Lock()
	st.inFlightFinalizes["/repo/wt"] = inFlightFinalize{
		cancel:    func() { cancelled = true },
		startedAt: time.Now(),
	}
	st.inFlightMu.Unlock()

	sweepExpiredFinalizes(ctx, st)

	if cancelled {
		t.Errorf("fresh in-flight finalize must not be cancelled")
	}
	evs, _ := st.Store.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtWorktreeCreateError},
		Limit:      5,
	})
	if len(evs) != 0 {
		t.Errorf("expected no synthetic event for fresh finalize, got %d", len(evs))
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
