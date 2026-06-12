package store

import (
	"database/sql"
	"encoding/json"
)

// Custom JSON marshaling for the event-log row types. Without these,
// `--json` output (and MCP tool results) leak Go internals: ALL-CAPS
// field names and sql.Null* wrappers like {"Int64":5,"Valid":true}.
// The shapes below are snake_case with nullable columns flattened to
// `null`-or-value, and payload_json inlined as a raw JSON object so
// consumers don't have to double-decode a string.
//
// Marshal-only by design: nothing unmarshals back into these structs
// (the RPC wire uses its own EventEnvelope), so no UnmarshalJSON
// counterparts exist.

// nullInt64 flattens sql.NullInt64 to *int64 for JSON output.
func nullInt64(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

// rawOrString returns s as inline raw JSON when it parses, or as a
// quoted string when it doesn't (defensive: payload_json defaults to
// '{}' but rows written by older versions may hold anything).
func rawOrString(s string) json.RawMessage {
	if s != "" && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, err := json.Marshal(s)
	if err != nil {
		// Marshal of a plain string can only fail on invalid UTF-8;
		// degrade to an empty object rather than corrupt the stream.
		return json.RawMessage("{}")
	}
	return b
}

// MarshalJSON renders an Event in the documented snake_case shape.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID             int64           `json:"id"`
		Ts             int64           `json:"ts"`
		Level          string          `json:"level"`
		RepoID         *int64          `json:"repo_id,omitempty"`
		WorktreeID     *int64          `json:"worktree_id,omitempty"`
		EventType      string          `json:"event_type"`
		Phase          string          `json:"phase,omitempty"`
		Message        string          `json:"message,omitempty"`
		Payload        json.RawMessage `json:"payload,omitempty"`
		DurationMs     *int64          `json:"duration_ms,omitempty"`
		WorktreeSlug   string          `json:"worktree_slug,omitempty"`
		WorktreeBranch string          `json:"worktree_branch,omitempty"`
		WorktreePath   string          `json:"worktree_path,omitempty"`
	}{
		ID:             e.ID,
		Ts:             e.Ts,
		Level:          e.Level,
		RepoID:         nullInt64(e.RepoID),
		WorktreeID:     nullInt64(e.WorktreeID),
		EventType:      e.EventType,
		Phase:          e.Phase,
		Message:        e.Message,
		Payload:        rawOrString(e.PayloadJSON),
		DurationMs:     nullInt64(e.DurationMs),
		WorktreeSlug:   e.WorktreeSlug,
		WorktreeBranch: e.WorktreeBranch,
		WorktreePath:   e.WorktreePath,
	})
}

// MarshalJSON renders a HookRun in the documented snake_case shape.
func (h HookRun) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID           int64  `json:"id"`
		WorktreeID   int64  `json:"worktree_id"`
		WorktreeSlug string `json:"worktree_slug,omitempty"`
		Phase        string `json:"phase"`
		GroupIdx     int64  `json:"group_idx"`
		Command      string `json:"command,omitempty"`
		StartedAt    int64  `json:"started_at"`
		FinishedAt   *int64 `json:"finished_at,omitempty"`
		ExitCode     *int64 `json:"exit_code,omitempty"`
		StdoutTail   string `json:"stdout_tail,omitempty"`
		StderrTail   string `json:"stderr_tail,omitempty"`
	}{
		ID:           h.ID,
		WorktreeID:   h.WorktreeID,
		WorktreeSlug: h.WorktreeSlug,
		Phase:        h.Phase,
		GroupIdx:     h.GroupIdx,
		Command:      h.Command,
		StartedAt:    h.StartedAt,
		FinishedAt:   nullInt64(h.FinishedAt),
		ExitCode:     nullInt64(h.ExitCode),
		StdoutTail:   h.StdoutTail,
		StderrTail:   h.StderrTail,
	})
}

// MarshalJSON renders a HookLogChunk with its body as a UTF-8 string
// (the default []byte encoding would base64 it).
func (c HookLogChunk) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID        int64  `json:"id"`
		HookRunID int64  `json:"hook_run_id"`
		Ts        int64  `json:"ts"`
		Stream    string `json:"stream"`
		Body      string `json:"body"`
	}{
		ID:        c.ID,
		HookRunID: c.HookRunID,
		Ts:        c.Ts,
		Stream:    c.Stream,
		Body:      string(c.Body),
	})
}
