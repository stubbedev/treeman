package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
)

// register installs a synthetic task runner for the duration of a test.
func register(t *testing.T, typ string, fn taskRunner) {
	t.Helper()
	taskRunners[typ] = fn
	t.Cleanup(func() { delete(taskRunners, typ) })
}

// TestExecutePlanSequentialWithinLaneStopsOnFailure verifies a group is
// one ordered lane: tasks run in order, and a failure halts the rest of
// that lane.
func TestExecutePlanSequentialWithinLaneStopsOnFailure(t *testing.T) {
	var mu sync.Mutex
	var order []string
	rec := func(name string, fail bool) taskRunner {
		return func(_ context.Context, _ *State, _ rpc.Task) (json.RawMessage, error) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			if fail {
				return nil, errors.New("boom")
			}
			return json.RawMessage(`"` + name + `"`), nil
		}
	}
	register(t, "__t_a", rec("a", false))
	register(t, "__t_fail", rec("fail", true))
	register(t, "__t_c", rec("c", false))

	results := ExecutePlan(context.Background(), &State{}, [][]rpc.Task{{
		{Type: "__t_a"}, {Type: "__t_fail"}, {Type: "__t_c"},
	}})

	if len(order) != 2 || order[0] != "a" || order[1] != "fail" {
		t.Fatalf("expected lane to run a→fail then stop, got %v", order)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (c skipped), got %d: %+v", len(results), results)
	}
	if !results[0].OK || results[1].OK {
		t.Errorf("expected [ok, fail], got %+v", results)
	}
}

// TestExecutePlanLanesRunInParallel proves separate groups run
// concurrently: two tasks rendezvous on channels and only complete if
// both are in flight at once. A sequential executor would deadlock.
func TestExecutePlanLanesRunInParallel(t *testing.T) {
	chA := make(chan struct{})
	chB := make(chan struct{})
	register(t, "__t_lane_a", func(_ context.Context, _ *State, _ rpc.Task) (json.RawMessage, error) {
		close(chA)
		select {
		case <-chB:
		case <-time.After(2 * time.Second):
			return nil, errors.New("lane A timed out waiting for lane B — not parallel")
		}
		return nil, nil
	})
	register(t, "__t_lane_b", func(_ context.Context, _ *State, _ rpc.Task) (json.RawMessage, error) {
		close(chB)
		select {
		case <-chA:
		case <-time.After(2 * time.Second):
			return nil, errors.New("lane B timed out waiting for lane A — not parallel")
		}
		return nil, nil
	})

	results := ExecutePlan(context.Background(), &State{}, [][]rpc.Task{
		{{Type: "__t_lane_a"}}, {{Type: "__t_lane_b"}},
	})
	for _, r := range results {
		if !r.OK {
			t.Fatalf("lane failed (not parallel?): %+v", results)
		}
	}
}

// TestExecutePlanResultsInSubmissionOrder verifies results come back
// lane-by-lane in submission order even when a later lane finishes first.
func TestExecutePlanResultsInSubmissionOrder(t *testing.T) {
	register(t, "__t_slow", func(_ context.Context, _ *State, _ rpc.Task) (json.RawMessage, error) {
		time.Sleep(80 * time.Millisecond)
		return json.RawMessage(`"slow"`), nil
	})
	register(t, "__t_fast", func(_ context.Context, _ *State, _ rpc.Task) (json.RawMessage, error) {
		return json.RawMessage(`"fast"`), nil
	})

	results := ExecutePlan(context.Background(), &State{}, [][]rpc.Task{
		{{Type: "__t_slow"}}, {{Type: "__t_fast"}},
	})
	if len(results) != 2 || results[0].Type != "__t_slow" || results[1].Type != "__t_fast" {
		t.Fatalf("results not in submission order: %+v", results)
	}
}

func TestRunOneTaskUnknownType(t *testing.T) {
	_, err := runOneTask(context.Background(), &State{}, rpc.Task{Type: "nope"})
	if err == nil {
		t.Fatal("expected error for unknown task type")
	}
}

// TestHandleRunPlanResultMode: Wait=true runs synchronously and returns
// the task payloads.
func TestHandleRunPlanResultMode(t *testing.T) {
	st := newTestState(t)
	register(t, "__t_payload", func(_ context.Context, _ *State, _ rpc.Task) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})
	resp := handleRunPlan(context.Background(), st, rpc.Plan(true, rpc.One(rpc.Task{Type: "__t_payload"})))
	if resp.Kind != rpc.KindPlanResult {
		t.Fatalf("kind: %s", resp.Kind)
	}
	if len(resp.TaskResults) != 1 || !resp.TaskResults[0].OK ||
		resp.TaskResults[0].PayloadJSON != `{"ok":true}` {
		t.Fatalf("results: %+v", resp.TaskResults)
	}
}

// TestHandleRunPlanQueuedMode: Wait=false returns KindPlanQueued at once
// and runs the plan asynchronously. We wait for the terminal
// run_plan_done event so the background goroutine is fully settled before
// the test's store closes.
func TestHandleRunPlanQueuedMode(t *testing.T) {
	st := newTestState(t)
	ran := make(chan struct{}, 1)
	register(t, "__t_async", func(_ context.Context, _ *State, _ rpc.Task) (json.RawMessage, error) {
		ran <- struct{}{}
		return nil, nil
	})
	resp := handleRunPlan(context.Background(), st, rpc.Plan(false, rpc.One(rpc.Task{Type: "__t_async"})))
	if resp.Kind != rpc.KindPlanQueued {
		t.Fatalf("kind: %s", resp.Kind)
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("queued task did not run asynchronously")
	}
	// Wait for the terminal event so the safeGo goroutine's final
	// WriteEvent completes before t.Cleanup closes the store.
	waitForEvent(t, st, store.EvtPlanEnd)
}

// waitForEvent polls the event log until an event of the given type
// appears or the deadline elapses.
func waitForEvent(t *testing.T, st *State, eventType string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evs, err := st.Store.QueryEvents(context.Background(), store.EventFilter{EventTypes: []string{eventType}, Limit: 1})
		if err == nil && len(evs) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("event %q never arrived", eventType)
}
