package store

import (
	"database/sql"
	"encoding/json"
)

// Custom JSON marshaling for the event-log row types. Without these,
// `--json` output (and MCP tool results) leak Go internals: ALL-CAPS
// field names and sql.Null* wrappers like {"Int64":5,"Valid":true}.
// The shapes below are snake_case with nullable columns flattened to
// `null`-or-value, and payload inlined as a raw JSON value so
// consumers don't have to double-decode a string.
//
// The wire shapes are NAMED, exported types (EventJSON, HookRunJSON,
// HookLogChunkJSON) rather than anonymous structs so the MCP layer can
// reflect a JSON Schema that matches the bytes on the wire. The SDK
// reflects the Go output type and validates results against it; a
// struct with a custom MarshalJSON whose emitted keys differ from the
// reflected field names trips `additionalProperties:false`. Returning
// the *JSON types (which carry no custom marshaler) keeps reflection
// and marshaling in lockstep.
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

// payloadValue decodes a payload_json string into the JSON value it
// represents, so it inlines as a raw object/array/scalar rather than a
// double-encoded string. payload_json defaults to '{}' but rows written
// by older versions may hold anything; non-JSON content degrades to the
// quoted string verbatim.
func payloadValue(s string) any {
	if s != "" && json.Valid([]byte(s)) {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return v
		}
	}
	return s
}

// EventJSON is the wire shape of an Event: snake_case keys, nullable
// columns flattened to `null`-or-value, payload inlined as raw JSON.
// Exported so the MCP layer can use it as a tool output type and get a
// reflected schema that matches these bytes.
type EventJSON struct {
	ID             int64  `json:"id"`
	Ts             int64  `json:"ts"`
	Level          string `json:"level"`
	RepoID         *int64 `json:"repo_id,omitempty"`
	WorktreeID     *int64 `json:"worktree_id,omitempty"`
	EventType      string `json:"event_type"`
	Phase          string `json:"phase,omitempty"`
	Message        string `json:"message,omitempty"`
	Payload        any    `json:"payload,omitempty"`
	DurationMs     *int64 `json:"duration_ms,omitempty"`
	WorktreeSlug   string `json:"worktree_slug,omitempty"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
	WorktreePath   string `json:"worktree_path,omitempty"`
}

// JSON renders an Event in its documented snake_case wire shape.
func (e Event) JSON() EventJSON {
	return EventJSON{
		ID:             e.ID,
		Ts:             e.Ts,
		Level:          e.Level,
		RepoID:         nullInt64(e.RepoID),
		WorktreeID:     nullInt64(e.WorktreeID),
		EventType:      e.EventType,
		Phase:          e.Phase,
		Message:        e.Message,
		Payload:        payloadValue(e.PayloadJSON),
		DurationMs:     nullInt64(e.DurationMs),
		WorktreeSlug:   e.WorktreeSlug,
		WorktreeBranch: e.WorktreeBranch,
		WorktreePath:   e.WorktreePath,
	}
}

// MarshalJSON renders an Event in the documented snake_case shape.
func (e Event) MarshalJSON() ([]byte, error) { return json.Marshal(e.JSON()) }

// EventsJSON converts a slice of Events to their wire shape.
func EventsJSON(es []Event) []EventJSON {
	out := make([]EventJSON, len(es))
	for i, e := range es {
		out[i] = e.JSON()
	}
	return out
}

// HookRunJSON is the wire shape of a HookRun.
type HookRunJSON struct {
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
}

// JSON renders a HookRun in its documented snake_case wire shape.
func (h HookRun) JSON() HookRunJSON {
	return HookRunJSON{
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
	}
}

// MarshalJSON renders a HookRun in the documented snake_case shape.
func (h HookRun) MarshalJSON() ([]byte, error) { return json.Marshal(h.JSON()) }

// HookRunsJSON converts a slice of HookRuns to their wire shape.
func HookRunsJSON(hs []HookRun) []HookRunJSON {
	out := make([]HookRunJSON, len(hs))
	for i, h := range hs {
		out[i] = h.JSON()
	}
	return out
}

// HookLogChunkJSON is the wire shape of a HookLogChunk, with its body
// as a UTF-8 string (the default []byte encoding would base64 it).
type HookLogChunkJSON struct {
	ID        int64  `json:"id"`
	HookRunID int64  `json:"hook_run_id"`
	Ts        int64  `json:"ts"`
	Stream    string `json:"stream"`
	Body      string `json:"body"`
}

// JSON renders a HookLogChunk in its documented wire shape.
func (c HookLogChunk) JSON() HookLogChunkJSON {
	return HookLogChunkJSON{
		ID:        c.ID,
		HookRunID: c.HookRunID,
		Ts:        c.Ts,
		Stream:    c.Stream,
		Body:      string(c.Body),
	}
}

// MarshalJSON renders a HookLogChunk with its body as a UTF-8 string.
func (c HookLogChunk) MarshalJSON() ([]byte, error) { return json.Marshal(c.JSON()) }
