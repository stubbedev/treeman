package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/rpc"
)

// TestDispatchDaemonState_EmptyShape — with no watchers / in-flight
// work / backoffs, the handler must return a populated DaemonState
// envelope (not nil) with zero counts. Guards against a regression
// that returns nil + KindError.
func TestDispatchDaemonState_EmptyShape(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	resp := Dispatch(context.Background(), st, make(chan struct{}, 1),
		rpc.Request{Method: rpc.MethodDaemonState})
	if resp.Kind != rpc.KindDaemonState {
		t.Fatalf("Kind = %s, want daemon_state", resp.Kind)
	}
	if resp.State == nil {
		t.Fatal("State is nil")
	}
	if resp.State.WatcherCount != 0 {
		t.Errorf("WatcherCount = %d, want 0", resp.State.WatcherCount)
	}
	if len(resp.State.InFlightFinalizes) != 0 {
		t.Errorf("InFlightFinalizes = %d, want 0", len(resp.State.InFlightFinalizes))
	}
}

// TestDispatchDaemonState_SurfacesInFlightFinalize — registering a
// finalize via MarkFinalizeInFlight should surface in the snapshot
// with a positive age. Otherwise the "is the daemon busy?" use case
// is broken.
func TestDispatchDaemonState_SurfacesInFlightFinalize(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	if !st.MarkFinalizeInFlight("/wt/test", func() {}) {
		t.Fatal("expected to mark in-flight successfully")
	}
	defer st.UnmarkFinalizeInFlight("/wt/test")
	// Sleep a tick so AgeSeconds will be >= 0 (could be 0 on a
	// nanosecond-fast machine; the assertion is just "appears").
	time.Sleep(2 * time.Millisecond)

	resp := Dispatch(context.Background(), st, make(chan struct{}, 1),
		rpc.Request{Method: rpc.MethodDaemonState})
	if resp.State == nil {
		t.Fatal("nil State")
	}
	if len(resp.State.InFlightFinalizes) != 1 {
		t.Fatalf("InFlightFinalizes = %d, want 1", len(resp.State.InFlightFinalizes))
	}
	got := resp.State.InFlightFinalizes[0]
	if got.WorktreePath != "/wt/test" {
		t.Errorf("WorktreePath = %q, want /wt/test", got.WorktreePath)
	}
	if got.StartedAtUnix == 0 {
		t.Errorf("StartedAtUnix not populated")
	}
}

// TestDispatchDaemonState_SurfacesSyncBackoff — record a sync failure
// and assert the backoff entry shows up with the same RepoPath +
// non-zero NextRetryUnix.
func TestDispatchDaemonState_SurfacesSyncBackoff(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	_ = st.RecordSyncFailure("/repo/x", 30*time.Second)
	resp := Dispatch(context.Background(), st, make(chan struct{}, 1),
		rpc.Request{Method: rpc.MethodDaemonState})
	if resp.State == nil {
		t.Fatal("nil State")
	}
	if len(resp.State.SyncBackoffs) != 1 {
		t.Fatalf("SyncBackoffs = %d, want 1", len(resp.State.SyncBackoffs))
	}
	b := resp.State.SyncBackoffs[0]
	if b.RepoPath != "/repo/x" {
		t.Errorf("RepoPath = %q", b.RepoPath)
	}
	if b.ConsecFailures != 1 {
		t.Errorf("ConsecFailures = %d, want 1", b.ConsecFailures)
	}
	if b.NextRetryUnix == 0 {
		t.Errorf("NextRetryUnix not populated")
	}
}
