package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

func waitTeardownHeartbeats(t *testing.T, st *store.Store, filter store.EventFilter) []store.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := st.QueryEvents(t.Context(), filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) >= 2 {
			return events
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("missing repeated engine drop heartbeat")
	return nil
}

func TestTeardownHeartbeatEmitsProgressAndStops(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		name := "completion"
		if cancelParent {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			st := newTestState(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			repoID, err := st.Store.EnsureRepo(ctx, t.TempDir(), "repo")
			if err != nil {
				t.Fatal(err)
			}
			wtID, err := st.Store.EnsureWorktree(ctx, repoID, t.TempDir(), "orphan", "feature")
			if err != nil {
				t.Fatal(err)
			}
			stop := startTeardownHeartbeat(ctx, st.Store, repoID, wtID, "orphan", 5*time.Millisecond)
			defer stop()
			filter := store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtDBTeardownProgress}, OldestFirst: true}
			events := waitTeardownHeartbeats(t, st.Store, filter)
			if events[0].Message != "engine drop beginning" || events[1].Phase != "engine-drop" {
				t.Fatalf("unexpected progress events: %+v", events)
			}
			var payload struct {
				Slug      string `json:"slug"`
				ElapsedMs int64  `json:"elapsed_ms"`
			}
			if err := json.Unmarshal([]byte(events[1].PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Slug != "orphan" || payload.ElapsedMs <= 0 || events[1].RepoID.Int64 != repoID {
				t.Fatalf("unexpected heartbeat payload or repo: %+v %+v", payload, events[1])
			}
			if cancelParent {
				cancel()
			}
			stop()
			events, err = st.Store.QueryEvents(t.Context(), filter)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
			after, err := st.Store.QueryEvents(t.Context(), filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(events) {
				t.Fatal("heartbeat continued after stopping")
			}
		})
	}
}
