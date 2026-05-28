package prepare

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/store"
)

// TestEmitPhaseDone_WritesEventWithPhaseAndDuration covers the
// post-step instrumentation each per-engine prepare uses to surface
// dump-load / migrate / seed / snapshot-create timings. The fields
// that matter for the `logs` CLI: phase, duration_ms, event_type,
// and the engine + source_db payload.
func TestEmitPhaseDone_WritesEventWithPhaseAndDuration(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")

	stepStart := time.Now().Add(-50 * time.Millisecond)
	emitPhaseDone(ctx, s, repoID, wtID, "mysql", "appdb", "migrate", stepStart)

	evs, err := s.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{"prepare_phase"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 prepare_phase event, got %d", len(evs))
	}
	e := evs[0]
	if e.Phase != "migrate" {
		t.Errorf("phase = %q, want migrate", e.Phase)
	}
	if !e.DurationMs.Valid || e.DurationMs.Int64 < 40 {
		t.Errorf("duration_ms = %v, want >= ~50ms", e.DurationMs)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(e.PayloadJSON), &payload); err != nil {
		t.Fatalf("payload not a string-map: %v (%s)", err, e.PayloadJSON)
	}
	if payload["engine"] != "mysql" || payload["source_db"] != "appdb" {
		t.Errorf("payload missing engine/source_db: %#v", payload)
	}
	if !strings.Contains(e.Message, "migrate") {
		t.Errorf("message should mention phase: %q", e.Message)
	}
}

// TestEmitPhaseDone_RunIDInjectedFromCtx — ctx carries a run_id, the
// payload picks it up via store.injectRunID. Acts as the integration
// test for the auto-injection wired into WriteEvent.
func TestEmitPhaseDone_RunIDInjectedFromCtx(t *testing.T) {
	ctx := runid.With(context.Background(), "deadbeef")
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")

	emitPhaseDone(ctx, s, repoID, wtID, "postgres", "appdb", "dump-load", time.Now())

	evs, _ := s.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{"prepare_phase"},
	})
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(evs[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["run_id"] != "deadbeef" {
		t.Errorf("run_id not auto-injected: %#v", payload)
	}
}

// TestEmitPhaseDone_NilStoreIsSafe — the helper is called from the
// prepare orchestrator which may be invoked with a nil store in unit
// contexts. A nil store must short-circuit without panicking.
func TestEmitPhaseDone_NilStoreIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emitPhaseDone(nil store) panicked: %v", r)
		}
	}()
	emitPhaseDone(context.Background(), nil, 0, 0, "mysql", "x", "seed", time.Now())
}
