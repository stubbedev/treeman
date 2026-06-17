package mcp

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

// validateAgainstSchema reflects T's output schema the same way registerTool
// does, then validates a marshaled instance against it — exactly the check the
// MCP SDK runs on every tool result. A custom MarshalJSON whose emitted keys
// diverge from the reflected struct fields fails here (the additionalProperties
// regression that broke logs_query / logs_hooks).
func validateAgainstSchema[T any](t *testing.T, out T) {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	resolved, err := schemaFor[T]().Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}
	if err := resolved.Validate(inst); err != nil {
		t.Fatalf("output does not satisfy its own reflected schema: %v\nwire bytes: %s", err, raw)
	}
}

func TestEventOutputMatchesSchema(t *testing.T) {
	events := []store.Event{
		{
			ID:             1,
			Ts:             1700000000000,
			Level:          "info",
			RepoID:         sql.NullInt64{Int64: 7, Valid: true},
			WorktreeID:     sql.NullInt64{Int64: 3, Valid: true},
			EventType:      "prepare.start",
			Phase:          "prepare",
			Message:        "starting",
			PayloadJSON:    `{"run_id":"abcd1234","count":5}`, // inlined object
			DurationMs:     sql.NullInt64{Int64: 42, Valid: true},
			WorktreeSlug:   "wt_deadbeef",
			WorktreeBranch: "feature/x",
			WorktreePath:   "/tmp/wt",
		},
		{
			ID:          2,
			Ts:          1700000001000,
			Level:       "error",
			EventType:   "prepare.fail",
			PayloadJSON: "", // empty payload — exercises the degrade path
			// nullable cols left invalid → must serialize as absent, not {}
		},
	}
	validateAgainstSchema(t, logsQueryOut{Events: store.EventsJSON(events)})
}

func TestHookRunOutputMatchesSchema(t *testing.T) {
	runs := []store.HookRun{
		{
			ID:           1,
			WorktreeID:   3,
			WorktreeSlug: "wt_deadbeef",
			Phase:        "create-before-engines",
			GroupIdx:     0,
			Command:      "npm ci",
			StartedAt:    1700000000000,
			FinishedAt:   sql.NullInt64{Int64: 1700000005000, Valid: true},
			ExitCode:     sql.NullInt64{Int64: 0, Valid: true},
			StdoutTail:   "ok",
			StderrTail:   "",
		},
		{
			ID:         2,
			WorktreeID: 3,
			Phase:      "create-before-engines",
			GroupIdx:   1,
			StartedAt:  1700000006000,
			// FinishedAt / ExitCode invalid → still running, must be absent
		},
	}
	validateAgainstSchema(t, logsHooksOut{Runs: store.HookRunsJSON(runs)})
}
