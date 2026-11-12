package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openRetryStore(t *testing.T) (*Store, int64) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repoID, err := s.EnsureRepo(ctx, "/r", "r")
	if err != nil {
		t.Fatal(err)
	}
	return s, repoID
}

func writeTaggedFailure(t *testing.T, s *Store, repoID, wtID int64, r CreateRetry) {
	t.Helper()
	payload := map[string]any{}
	r.AddTo(payload)
	if err := s.WriteEvent(context.Background(), LevelError, EvtWorktreeCreateError, "failed",
		repoID, wtID, PhaseBeforeEngines, 0, payload); err != nil {
		t.Fatal(err)
	}
}

func TestPendingCreateRetries(t *testing.T) {
	ctx := context.Background()
	s, repoID := openRetryStore(t)
	tagged, _ := s.EnsureWorktree(ctx, repoID, "/r/tagged", "tagged", "a")
	superseded, _ := s.EnsureWorktree(ctx, repoID, "/r/superseded", "superseded", "b")
	untagged, _ := s.EnsureWorktree(ctx, repoID, "/r/untagged", "untagged", "c")
	deleted, _ := s.EnsureWorktree(ctx, repoID, "/r/deleted", "deleted", "d")

	writeTaggedFailure(t, s, repoID, tagged, CreateRetry{Cause: RetryEnvLoadTimeout, Attempt: 1})
	writeTaggedFailure(t, s, repoID, superseded, CreateRetry{Cause: RetryEnvLoadTimeout})
	_ = s.WriteEvent(ctx, LevelInfo, EvtWorktreeCreateStart, "manual finalize", repoID, superseded, "", 0, nil)
	_ = s.WriteEvent(ctx, LevelError, EvtWorktreeCreateError, "migrate failed", repoID, untagged, "", 0, nil)
	writeTaggedFailure(t, s, repoID, deleted, CreateRetry{Cause: RetryEnvLoadTimeout})
	if err := s.MarkWorktreeDeleted(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	s.FlushEvents(ctx)

	got, err := s.PendingCreateRetries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Worktree.ID != tagged {
		t.Fatalf("want only the tagged worktree, got %+v", got)
	}
	if r := got[0].Retry; r.Cause != RetryEnvLoadTimeout || r.Attempt != 1 {
		t.Errorf("retry = %+v", r)
	}
}

func TestCreateRetryOfRejectsUnknownCause(t *testing.T) {
	_, _, err := CreateRetryOf(Event{
		EventType: EvtWorktreeCreateError, Level: LevelError,
		PayloadJSON: `{"retry":{"cause":"bogus","attempt":0}}`,
	})
	if err == nil {
		t.Error("an unknown retry cause must be an error, not silently retried or dropped")
	}
}

// Regression for #198: a standalone prepare after a failed setup ran
// migrate without vendor/. SetupFailure reads the newest lifecycle op,
// which a standalone prepare:end does not replace.
func TestSetupFailureSurvivesStandalonePrepare(t *testing.T) {
	ctx := context.Background()
	s, repoID := openRetryStore(t)
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/wt", "wt", "a")
	_ = s.WriteEvent(ctx, LevelInfo, EvtWorktreeCreateStart, "create", repoID, wtID, "", 0, nil)
	_ = s.WriteEvent(ctx, LevelError, EvtWorktreeCreateError, "hook environment: timed out", repoID, wtID, PhaseBeforeEngines, 0, nil)
	_ = s.WriteEvent(ctx, LevelInfo, EvtPrepareEnd, "prepare", repoID, wtID, "", 0, nil)

	e, failed, err := s.SetupFailure(ctx, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if !failed || e.Message != "hook environment: timed out" {
		t.Fatalf("want the before-engines failure, got failed=%v %+v", failed, e)
	}

	_ = s.WriteEvent(ctx, LevelInfo, EvtWorktreeCreateStart, "finalize again", repoID, wtID, "", 0, nil)
	if _, failed, err := s.SetupFailure(ctx, wtID); err != nil || failed {
		t.Errorf("a new finalize reopens setup: failed=%v err=%v", failed, err)
	}
}

func TestCreateRetryOfRequiresDatabasesForEngineUnreachable(t *testing.T) {
	_, _, err := CreateRetryOf(Event{
		EventType: EvtWorktreeCreateError, Level: LevelError,
		PayloadJSON: `{"retry":{"cause":"engine_unreachable","attempt":0}}`,
	})
	if err == nil {
		t.Error("an engine-unreachable retry without databases cannot be probed and must be rejected")
	}
}
