package store

import (
	"context"
	"fmt"
)

// Status buckets a worktree's lifecycle state falls into, as shown by
// `treeman status`, the MCP status tool and the notification filter.
const (
	BucketStable = "stable"
	BucketUp     = "up"
	BucketDown   = "down"
	BucketFailed = "failed"
)

// Worktree lifecycle states derived by DeriveWorktreeState.
const (
	StateReady     = "ready"
	StatePreparing = "preparing"
	StateDeferred  = "deferred"
	StateError     = "error"
	StateTeardown  = "teardown"
)

const PhaseBeforeEngines = "before-engines"

// WorktreeOpEventTypes are the lifecycle-operation events: a full
// finalize, a watch re-prepare, a teardown or a reap. Its start event
// opens the operation and its terminals (end/error/deferred) close it.
var WorktreeOpEventTypes = []string{
	EvtWorktreeCreateStart, EvtWorktreeCreateEnd, EvtWorktreeCreateError,
	EvtWorktreeCreateDeferred,
	EvtWorktreeDeleteStart, EvtWorktreeDeleteEnd, EvtWorktreeDeleteError,
	EvtWorktreeReapStart, EvtWorktreeReapEnd, EvtWorktreeReapError,
}

// PrepareStateEventTypes are the terminals of a STANDALONE prepare run
// (MCP, plan tasks): with no lifecycle op bracketing it, a fresh
// prepare:end clears an earlier failure and a prepare:error reports one.
var PrepareStateEventTypes = []string{EvtPrepareEnd, EvtPrepareError}

// WorktreeStateEventTypes is the union of every event type that can
// define a worktree's state; see WorktreeStateEvent for which one wins.
var WorktreeStateEventTypes = append(
	append(make([]string, 0, len(WorktreeOpEventTypes)+len(PrepareStateEventTypes)), WorktreeOpEventTypes...),
	PrepareStateEventTypes...,
)

// DeriveWorktreeState maps a worktree's state-defining event (the zero
// Event when it has none) to its state and bucket.
func DeriveWorktreeState(last Event) (state, bucket string) {
	switch last.EventType {
	case EvtWorktreeCreateStart:
		return StatePreparing, BucketUp
	case EvtWorktreeCreateDeferred:
		return StateDeferred, BucketStable
	case EvtWorktreeCreateError, EvtPrepareError, EvtWorktreeDeleteError, EvtWorktreeReapError:
		if last.Level == LevelError {
			return StateError, BucketFailed
		}
	case EvtWorktreeDeleteStart, EvtWorktreeReapStart:
		return StateTeardown, BucketDown
	}
	return StateReady, BucketStable
}

// WorktreeStateEvent picks the event that defines a worktree's state from
// its newest lifecycle-op event and its newest standalone prepare
// terminal.
//
// A prepare:end/prepare:error only speaks for the worktree once no
// lifecycle op owns it. An in-flight finalize emits one per database
// (prepare.Run fans them out in parallel), so the first database to
// finish would otherwise flip the worktree to `ready` while the rest —
// notably a mysql fan-out still cloning the per-test databases — are
// still building (#180). Only the op's own terminal event closes it.
func WorktreeStateEvent(latestOp, latestPrepareTerminal Event) Event {
	if opInFlight(latestOp) || latestPrepareTerminal.ID <= latestOp.ID {
		return latestOp
	}
	return latestPrepareTerminal
}

// opInFlight reports whether e opened a lifecycle op that no terminal
// event has closed yet.
func opInFlight(e Event) bool {
	switch e.EventType {
	case EvtWorktreeCreateStart, EvtWorktreeDeleteStart, EvtWorktreeReapStart:
		return true
	}
	return false
}

func FinalizeFailurePhase(enginesTouched bool) string {
	if enginesTouched {
		return ""
	}
	return PhaseBeforeEngines
}

func FailedBeforeEngines(last Event) bool {
	return last.EventType == EvtWorktreeCreateError && last.Level == LevelError && last.Phase == PhaseBeforeEngines
}

// LatestWorktreeStates returns the state-defining event of every listed
// worktree (absent when it has no lifecycle event at all). Two batched
// queries — the newest op event and the newest standalone prepare
// terminal — reconcile per worktree through WorktreeStateEvent.
func (s *Store) LatestWorktreeStates(ctx context.Context, wtIDs []int64) (map[int64]Event, error) {
	ops, err := s.LatestEventPerWorktree(ctx, wtIDs, WorktreeOpEventTypes)
	if err != nil {
		return nil, fmt.Errorf("latest lifecycle op of worktrees %v: %w", wtIDs, err)
	}
	preps, err := s.LatestEventPerWorktree(ctx, wtIDs, PrepareStateEventTypes)
	if err != nil {
		return nil, fmt.Errorf("latest standalone prepare terminal of worktrees %v: %w", wtIDs, err)
	}
	out := make(map[int64]Event, len(wtIDs))
	for id, op := range ops {
		out[id] = WorktreeStateEvent(op, preps[id])
	}
	for id, prep := range preps {
		if _, ok := ops[id]; !ok {
			out[id] = prep
		}
	}
	return out, nil
}

func (s *Store) LatestWorktreeStateEvent(ctx context.Context, wtID int64) (Event, error) {
	latest, err := s.LatestWorktreeStates(ctx, []int64{wtID})
	if err != nil {
		return Event{}, fmt.Errorf("latest lifecycle event of worktree %d: %w", wtID, err)
	}
	return latest[wtID], nil
}
