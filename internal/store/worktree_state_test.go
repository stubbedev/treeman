package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWorktreeStateEvent(t *testing.T) {
	op := func(id int64, eventType, level string) Event {
		return Event{ID: id, EventType: eventType, Level: level}
	}
	tests := []struct {
		name      string
		op        Event
		prepare   Event
		wantType  string
		wantState string
	}{
		{
			name:      "per-database prepare:end during a finalize keeps the worktree preparing",
			op:        op(1, EvtWorktreeCreateStart, LevelInfo),
			prepare:   op(2, EvtPrepareEnd, LevelInfo),
			wantType:  EvtWorktreeCreateStart,
			wantState: StatePreparing,
		},
		{
			name:      "per-database prepare:error during a finalize does not surface a failure",
			op:        op(1, EvtWorktreeCreateStart, LevelInfo),
			prepare:   op(2, EvtPrepareError, LevelError),
			wantType:  EvtWorktreeCreateStart,
			wantState: StatePreparing,
		},
		{
			name:      "finalize terminal wins over an earlier prepare:end",
			op:        op(3, EvtWorktreeCreateEnd, LevelInfo),
			prepare:   op(2, EvtPrepareEnd, LevelInfo),
			wantType:  EvtWorktreeCreateEnd,
			wantState: StateReady,
		},
		{
			name:      "standalone prepare:end after the last finalize reads ready",
			op:        op(1, EvtWorktreeCreateError, LevelError),
			prepare:   op(2, EvtPrepareEnd, LevelInfo),
			wantType:  EvtPrepareEnd,
			wantState: StateReady,
		},
		{
			name:      "standalone prepare:error after the last finalize reads failed",
			op:        op(1, EvtWorktreeCreateEnd, LevelInfo),
			prepare:   op(2, EvtPrepareError, LevelError),
			wantType:  EvtPrepareError,
			wantState: StateError,
		},
		{
			name:      "prepare terminal without any lifecycle op defines the state",
			prepare:   op(2, EvtPrepareEnd, LevelInfo),
			wantType:  EvtPrepareEnd,
			wantState: StateReady,
		},
		{
			name:      "teardown in flight hides a later prepare terminal",
			op:        op(4, EvtWorktreeDeleteStart, LevelInfo),
			prepare:   op(5, EvtPrepareEnd, LevelInfo),
			wantType:  EvtWorktreeDeleteStart,
			wantState: StateTeardown,
		},
		{
			name:      "no events at all reads ready",
			wantState: StateReady,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorktreeStateEvent(tt.op, tt.prepare)
			if got.EventType != tt.wantType {
				t.Errorf("state event = %q (id %d), want %q", got.EventType, got.ID, tt.wantType)
			}
			if state, _ := DeriveWorktreeState(got); state != tt.wantState {
				t.Errorf("state = %q, want %q", state, tt.wantState)
			}
		})
	}
}

func TestLatestWorktreeStates(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	inFlight, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/in-flight", "in-flight", "b")
	settled, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/settled", "settled", "b")
	standalone, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/standalone", "standalone", "b")
	nothing, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/nothing", "nothing", "b")

	// A finalize whose database prepares finished but whose fan-out has not.
	_ = st.WriteEvent(ctx, LevelInfo, EvtWorktreeCreateStart, "finalize beginning", repoID, inFlight, "", 0, nil)
	_ = st.WriteEvent(ctx, LevelInfo, EvtPrepareEnd, "engine=mysql cache_hit", repoID, inFlight, "", 0, nil)
	// A finished finalize followed by an unrelated database prepare event.
	_ = st.WriteEvent(ctx, LevelInfo, EvtWorktreeCreateEnd, "finalize complete", repoID, settled, "", 0, nil)
	_ = st.WriteEvent(ctx, LevelInfo, EvtPrepareEnd, "engine=redis noop", repoID, settled, "", 0, nil)
	// A standalone prepare run with no lifecycle op around it.
	_ = st.WriteEvent(ctx, LevelError, EvtPrepareError, "migrate failed", repoID, standalone, "", 0, nil)
	_ = st.WriteEvent(ctx, LevelInfo, EvtPrepareEnd, "rerun ok", repoID, standalone, "", 0, nil)

	states, err := st.LatestWorktreeStates(ctx, []int64{inFlight, settled, standalone, nothing})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := states[inFlight].EventType, EvtWorktreeCreateStart; got != want {
		t.Errorf("in-flight event = %q, want the still-open %q", got, want)
	}
	if state, _ := DeriveWorktreeState(states[inFlight]); state != StatePreparing {
		t.Errorf("in-flight state = %q, want %q", state, StatePreparing)
	}
	if state, _ := DeriveWorktreeState(states[settled]); state != StateReady {
		t.Errorf("settled state = %q, want %q", state, StateReady)
	}
	if got, want := states[standalone].EventType, EvtPrepareEnd; got != want {
		t.Errorf("standalone event = %q, want the newer %q", got, want)
	}
	if state, _ := DeriveWorktreeState(states[standalone]); state != StateReady {
		t.Errorf("standalone state = %q, want %q", state, StateReady)
	}
	if _, ok := states[nothing]; ok {
		t.Error("worktree without lifecycle events must be absent from the map")
	}
}
