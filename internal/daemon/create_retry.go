package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stubbedev/treeman/internal/db/reachability"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/shellenv"
	"github.com/stubbedev/treeman/internal/store"
)

// createRetryAttempts bounds the automatic re-runs of one failed create.
const createRetryAttempts = 3

// createRetryBaseDelay is the wait before the first automatic re-run;
// each further one waits four times longer (30s, 2m, 8m).
const createRetryBaseDelay = 30 * time.Second

// createRetryProbeWindow is how long after an engine-unreachable
// failure the daemon keeps probing the engine before it gives up on the
// re-run.
const createRetryProbeWindow = 30 * time.Minute

// createRetryProbeTimeout bounds one probe of the unreachable engines.
const createRetryProbeTimeout = 30 * time.Second

func createRetryDelay(attempt int) time.Duration {
	return createRetryBaseDelay << (2 * attempt)
}

// transientCause classifies a finalize failure as one a later re-run
// can get past without anyone changing anything, and returns its tag
// without the attempt number.
func transientCause(err error) (store.CreateRetry, bool) {
	if de, ok := errors.AsType[*shellenv.DirenvError](err); ok && de.Transient() {
		return store.CreateRetry{Cause: store.RetryEnvLoadTimeout}, true
	}
	if dbs, ok := refusedDatabases(err); ok {
		return store.CreateRetry{Cause: store.RetryEngineUnreachable, DBs: dbs}, true
	}
	return store.CreateRetry{}, false
}

// refusedDatabases returns the indices of the databases a prepare failed
// on when every one of them failed because its engine refused the
// connection (down or still starting).
func refusedDatabases(err error) ([]int, bool) {
	re, ok := errors.AsType[*prepare.RunError](err)
	if !ok || len(re.Failed) == 0 {
		return nil, false
	}
	dbs := make([]int, len(re.Failed))
	for i, f := range re.Failed {
		if !reachability.IsRefused(f.Err) {
			return nil, false
		}
		dbs[i] = f.Index
	}
	return dbs, true
}

// createRetryFor returns the retry tag for a finalize failure of the
// run with the given automatic re-run number, false when the failure is
// not transient or the re-runs are used up.
func createRetryFor(err error, attempt int) (store.CreateRetry, bool) {
	r, ok := transientCause(err)
	if !ok || attempt >= createRetryAttempts {
		return store.CreateRetry{}, false
	}
	r.Attempt = attempt
	return r, true
}

func retryHint(r store.CreateRetry) string {
	if r.Cause == store.RetryEngineUnreachable {
		return fmt.Sprintf("hint: treemand re-runs this finalize once the engine answers, "+
			"probing from %s on for %s (re-run %d of %d); `treeman worktree finalize` retries it now",
			createRetryDelay(r.Attempt), createRetryProbeWindow, r.Attempt+1, createRetryAttempts)
	}
	return fmt.Sprintf("hint: treemand re-runs this finalize automatically in %s (re-run %d of %d); "+
		"`treeman worktree finalize` retries it now",
		createRetryDelay(r.Attempt), r.Attempt+1, createRetryAttempts)
}

// retryDue reports whether p's back-off has elapsed at now. An
// engine-unreachable retry stays due, probed on every tick, until its
// probe window closes.
func retryDue(p store.PendingCreateRetry, now time.Time) bool {
	elapsed := now.Sub(time.UnixMilli(p.Failure.Ts))
	delay := createRetryDelay(p.Retry.Attempt)
	if elapsed < delay {
		return false
	}
	return p.Retry.Cause != store.RetryEngineUnreachable || elapsed < delay+createRetryProbeWindow
}

// sweepCreateRetries re-runs the finalize of every active worktree whose
// newest lifecycle op is a create failure tagged for retry, once its
// back-off elapsed. The state lives in the event log, so retries survive
// a daemon restart and stop as soon as anything else (a manual finalize,
// a teardown) runs on the worktree. A worktree with a finalize or
// teardown in flight is left alone; the re-run claims the finalize slot
// like any other, so a teardown preempts it as usual.
func sweepCreateRetries(ctx context.Context, st *State) {
	pending, err := st.Store.PendingCreateRetries(ctx)
	if err != nil {
		slog.Warn("create-retry sweep: list failed creates", "err", err)
		return
	}
	for _, p := range dueCreateRetries(pending, time.Now(), st.createRetryBlocked) {
		launchCreateRetry(st, p)
	}
}

// dueCreateRetries keeps the pending retries whose back-off elapsed at
// now and whose worktree blocked does not report busy.
func dueCreateRetries(pending []store.PendingCreateRetry, now time.Time, blocked func(wtPath string) bool) []store.PendingCreateRetry {
	var due []store.PendingCreateRetry
	for _, p := range pending {
		if retryDue(p, now) && !blocked(p.Worktree.Path) {
			due = append(due, p)
		}
	}
	return due
}

// createRetryBlocked reports whether a teardown or another finalize of
// wtPath is in flight, so an automatic re-run must not start.
func (st *State) createRetryBlocked(wtPath string) bool {
	return st.IsTeardownInFlight(wtPath) || st.IsFinalizeInFlight(wtPath)
}

func launchCreateRetry(st *State, p store.PendingCreateRetry) {
	wt := p.Worktree
	attempt := p.Retry.Attempt + 1
	slog.Info("re-running failed create", "wt", wt.Path, "cause", p.Retry.Cause, "attempt", attempt)
	safeGo(lblCreateRetry, wt.Path, func() {
		ctx := withTrigger(runid.With(st.BgCtx, runid.New()), trigger{Source: triggerCreateRetry, Attempt: attempt})
		if p.Retry.Cause == store.RetryEngineUnreachable && !enginesAnswer(ctx, wt, p.Retry.DBs) {
			return
		}
		env, err := st.Store.LoadInheritedEnv(ctx, wt.ID)
		if err != nil {
			slog.Warn("create retry: load the worktree's env", "wt", wt.Path, "err", err)
			return
		}
		if err := FinalizeWorktree(ctx, st, wt.RepoPath, wt.Path, env, false); err != nil {
			slog.Warn("create retry failed", "wt", wt.Path, "attempt", attempt, "err", err)
		}
	})
}

// enginesAnswer probes the engines of the databases an engine-unreachable
// create failed on. Only a refusal holds the re-run back: any other
// failure (the config changed, a different error) lets the re-run
// surface it.
func enginesAnswer(ctx context.Context, wt store.WorktreeRow, dbs []int) bool {
	cfg, err := resolve.LoadResolvedForWorktree(wt.RepoPath, wt.Path)
	if err != nil {
		slog.Warn("create retry: load config to probe engines; re-running", "wt", wt.Path, "err", err)
		return true
	}
	probeCtx, cancel := context.WithTimeout(ctx, createRetryProbeTimeout)
	defer cancel()
	err = prepare.ProbeDatabases(probeCtx, &cfg, dbs)
	if reachability.IsRefused(err) {
		slog.Debug("create retry: engine still unreachable", "wt", wt.Path, "dbs", dbs, "err", err)
		return false
	}
	return true
}
