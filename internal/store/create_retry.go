package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// RetryCause names the transient condition a failed create waits out
// before the daemon re-runs its finalize on its own.
type RetryCause string

const (
	// RetryEnvLoadTimeout: loading the worktree's direnv environment
	// timed out, typically a cold nix build that keeps running in the
	// nix daemon and is done (or further along) on the next attempt.
	RetryEnvLoadTimeout RetryCause = "env_load_timeout"
	// RetryEngineUnreachable: an engine was down or still starting (the
	// connect refused or timed out, its container stopped, the handshake
	// dropped). The re-run waits until the engines of DBs answer.
	RetryEngineUnreachable RetryCause = "engine_unreachable"
	// RetryDaemonRestart: treemand restarted while the create was in
	// flight; nothing about the worktree caused the failure.
	RetryDaemonRestart RetryCause = "daemon_restart"
)

func (c RetryCause) valid() bool {
	switch c {
	case RetryEnvLoadTimeout, RetryEngineUnreachable, RetryDaemonRestart:
		return true
	}
	return false
}

// CreateRetry tags a worktree:create:error the daemon re-runs
// automatically. It lives in the event's payload under "retry".
type CreateRetry struct {
	Cause RetryCause `json:"cause"`
	// Attempt counts the automatic re-runs made before this failure:
	// 0 for a failed create, n for the n-th automatic re-run failing.
	Attempt int `json:"attempt"`
	// DBs lists the `databases[]` indices whose engine was unreachable;
	// set for RetryEngineUnreachable only.
	DBs []int `json:"dbs,omitempty"`
}

const payloadRetryKey = "retry"

// AddTo records r in an event payload.
func (r CreateRetry) AddTo(payload map[string]any) {
	payload[payloadRetryKey] = r
}

// CreateRetryOf returns the retry tag of e. ok is false when e is not an
// error-level worktree:create:error carrying one.
func CreateRetryOf(e Event) (r CreateRetry, ok bool, err error) {
	if e.EventType != EvtWorktreeCreateError || e.Level != LevelError {
		return CreateRetry{}, false, nil
	}
	var p struct {
		Retry *CreateRetry `json:"retry"`
	}
	if err := json.Unmarshal([]byte(e.PayloadJSON), &p); err != nil {
		return CreateRetry{}, false, fmt.Errorf("decode the payload of event %d: %w", e.ID, err)
	}
	if p.Retry == nil {
		return CreateRetry{}, false, nil
	}
	if !p.Retry.Cause.valid() {
		return CreateRetry{}, false, fmt.Errorf("event %d: unknown retry cause %q", e.ID, p.Retry.Cause)
	}
	if p.Retry.Cause == RetryEngineUnreachable && len(p.Retry.DBs) == 0 {
		return CreateRetry{}, false, fmt.Errorf("event %d: an engine-unreachable retry names no database", e.ID)
	}
	return *p.Retry, true, nil
}

// PendingCreateRetry is an active worktree whose latest lifecycle op is
// a create failure tagged for an automatic retry.
type PendingCreateRetry struct {
	Worktree WorktreeRow
	Failure  Event
	Retry    CreateRetry
}

// PendingCreateRetries returns every active worktree (not deleted, not
// tearing down) whose newest lifecycle op is a tagged create failure. A
// later finalize, watch re-prepare or teardown of the worktree supersedes
// the failure, so it drops out on its own. Two batched queries, both
// index-served: the active-row listing and the newest op event per
// worktree (idx_events_worktree_recent).
func (s *Store) PendingCreateRetries(ctx context.Context) ([]PendingCreateRetry, error) {
	rows, err := s.ListActiveWorktreeRows(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	latest, err := s.LatestEventPerWorktree(ctx, ids, WorktreeOpEventTypes)
	if err != nil {
		return nil, fmt.Errorf("latest lifecycle op of active worktrees: %w", err)
	}
	var out []PendingCreateRetry
	for _, row := range rows {
		e, ok := latest[row.ID]
		if !ok {
			continue
		}
		r, tagged, err := CreateRetryOf(e)
		if err != nil {
			return nil, fmt.Errorf("worktree %s: %w", row.Path, err)
		}
		if tagged {
			out = append(out, PendingCreateRetry{Worktree: row, Failure: e, Retry: r})
		}
	}
	return out, nil
}

// SetupFailure returns the worktree:create:error that left wtID's setup
// (its create-before-engines step) unfinished, when no lifecycle op has
// run since. It reads the newest lifecycle op only: a standalone
// prepare run since then does not count as setup having succeeded.
// Pending batched events are flushed first, so an op the caller just
// opened is seen.
func (s *Store) SetupFailure(ctx context.Context, wtID int64) (Event, bool, error) {
	s.FlushEvents(ctx)
	latest, err := s.LatestEventPerWorktree(ctx, []int64{wtID}, WorktreeOpEventTypes)
	if err != nil {
		return Event{}, false, fmt.Errorf("latest lifecycle op of worktree %d: %w", wtID, err)
	}
	e := latest[wtID]
	return e, FailedBeforeEngines(e), nil
}
