package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/shellenv"
	"github.com/stubbedev/treeman/internal/store"
)

// envLoadFailure builds the error chain a failed direnv load reaches the
// pipeline through: hooks' "hook environment: %w", then
// runTriggerActions' "<trigger>: %w".
func envLoadFailure(failure shellenv.DirenvFailure) error {
	de := &shellenv.DirenvError{
		Dir: "/wt", Bin: "/bin/direnv", Failure: failure,
		Elapsed: 10 * time.Minute, Limit: 10 * time.Minute, Err: errors.New("signal: killed"),
	}
	return fmt.Errorf("create-before-engines: %w", fmt.Errorf("hook environment: %w", de))
}

// Regression for #198: a direnv load that died before any hook ran got
// the hint about moving DB-touching hooks to create-after-engines.
func TestBeforeEnginesFailureHintMatchesTheFailure(t *testing.T) {
	envErr := beforeEnginesFailure(envLoadFailure(shellenv.DirenvTimedOut)).Error()
	if strings.Contains(envErr, "create-after-engines") || !strings.Contains(envErr, "before any create-before-engines hook ran") {
		t.Errorf("an env-load failure must say no hook ran, got %q", envErr)
	}
	hookErr := beforeEnginesFailure(errors.New("create-before-engines: exit 1")).Error()
	if !strings.Contains(hookErr, "move any app-boot / DB-touching step to create-after-engines") {
		t.Errorf("a failed hook keeps the before-engines contract hint, got %q", hookErr)
	}
}

func TestCreateRetryForIsBounded(t *testing.T) {
	timedOut := envLoadFailure(shellenv.DirenvTimedOut)
	r, ok := createRetryFor(timedOut, 0)
	if !ok || r.Cause != store.RetryEnvLoadTimeout || r.Attempt != 0 {
		t.Errorf("a timed-out env load must be retried, got %+v ok=%v", r, ok)
	}
	if _, ok := createRetryFor(timedOut, createRetryAttempts); ok {
		t.Errorf("the re-runs must stop after %d attempts", createRetryAttempts)
	}
	if _, ok := createRetryFor(envLoadFailure(shellenv.DirenvFailed), 0); ok {
		t.Error("a blocked or broken .envrc must not be retried")
	}
	if _, ok := createRetryFor(errors.New("migrate failed"), 0); ok {
		t.Error("an ordinary failure must not be retried")
	}
}

func TestRetryDueAfterBackOff(t *testing.T) {
	failedAt := time.Now()
	p := store.PendingCreateRetry{
		Failure: store.Event{Ts: failedAt.UnixMilli()},
		Retry:   store.CreateRetry{Cause: store.RetryEnvLoadTimeout, Attempt: 1},
	}
	if retryDue(p, failedAt.Add(createRetryDelay(1)-time.Second)) {
		t.Error("a retry must wait out its back-off")
	}
	if !retryDue(p, failedAt.Add(createRetryDelay(1))) {
		t.Error("a retry is due once its back-off elapsed")
	}
}

// Regression for #198: nothing re-ran a create whose env load timed out
// during a cold nix build. The failure event now carries the retry tag
// the sweep picks up.
func TestRecordFinalizeFailureTagsTransientFailures(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()
	repoID, _ := st.Store.EnsureRepo(ctx, "/repo", "repo")
	wtID, _ := st.Store.EnsureWorktree(ctx, repoID, "/repo/wt", "slug", "feature")

	recordFinalizeFailure(ctx, st, &config.Config{}, finalizeFailure{
		repoRoot: "/repo", wtRoot: "/repo/wt", repoID: repoID, wtID: wtID,
		phase: store.FinalizeSetup, err: beforeEnginesFailure(envLoadFailure(shellenv.DirenvTimedOut)),
	})

	pending, err := st.Store.PendingCreateRetries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Worktree.ID != wtID || pending[0].Retry.Cause != store.RetryEnvLoadTimeout {
		t.Fatalf("want one env-load retry pending for the worktree, got %+v", pending)
	}
	if !strings.Contains(pending[0].Failure.Message, "re-runs this finalize automatically") {
		t.Errorf("the failure must say it will be retried, got %q", pending[0].Failure.Message)
	}
}

func TestDueCreateRetriesSkipsBusyAndEarlyOnes(t *testing.T) {
	now := time.Now()
	pending := func(path string, failedAt time.Time) store.PendingCreateRetry {
		return store.PendingCreateRetry{
			Worktree: store.WorktreeRow{Path: path},
			Failure:  store.Event{Ts: failedAt.UnixMilli()},
			Retry:    store.CreateRetry{Cause: store.RetryEnvLoadTimeout},
		}
	}
	old := now.Add(-time.Hour)
	due := dueCreateRetries([]store.PendingCreateRetry{
		pending("/due", old),
		pending("/early", now),
		pending("/busy", old),
	}, now, func(p string) bool { return p == "/busy" })
	if len(due) != 1 || due[0].Worktree.Path != "/due" {
		t.Errorf("want only /due, got %+v", due)
	}
}

func TestCreateRetryBlockedByTeardownAndFinalize(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	if st.createRetryBlocked("/wt") {
		t.Fatal("an idle worktree must not be blocked")
	}
	if !st.MarkTeardownInFlight("/wt") {
		t.Fatal("mark teardown")
	}
	if !st.createRetryBlocked("/wt") {
		t.Error("a worktree being torn down must not be re-queued")
	}
	st.UnmarkTeardownInFlight("/wt")
	if !st.MarkFinalizeInFlight("/wt", nil) {
		t.Fatal("mark finalize")
	}
	if !st.createRetryBlocked("/wt") {
		t.Error("a worktree with a finalize in flight must not be re-queued")
	}
}
