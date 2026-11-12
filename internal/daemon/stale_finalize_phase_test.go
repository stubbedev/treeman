package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/store"
)

// Regression for #211: the restart sweep records which phase of the
// finalize it interrupted, so a restart during the create-before-engines
// hooks counts as a setup failure for the setup gate, leaves the
// engines alone, and is re-run by the create-retry sweep.
func TestSweepStalePreparingRecordsTheInterruptedPhase(t *testing.T) {
	cases := []struct {
		name         string
		trigger      trigger
		progress     []store.Event
		wantPhase    store.FinalizePhase
		wantRetry    bool
		wantRecovery bool
	}{
		{
			name:      "before any hook",
			trigger:   trigger{Source: triggerRPC},
			wantPhase: store.FinalizeSetup, wantRetry: true,
		},
		{
			name:      "during create-before-engines hooks",
			trigger:   trigger{Source: triggerRPC},
			progress:  []store.Event{{EventType: store.EvtHooksStart, Phase: hooks.CreateBeforeEngines.String()}},
			wantPhase: store.FinalizeSetup, wantRetry: true,
		},
		{
			name:    "during engine prepare",
			trigger: trigger{Source: triggerRPC},
			progress: []store.Event{
				{EventType: store.EvtHooksStart, Phase: hooks.CreateBeforeEngines.String()},
				{EventType: store.EvtPrepareStart},
			},
			wantPhase: store.FinalizeEngines, wantRetry: true, wantRecovery: true,
		},
		{
			name:    "during create-after-engines hooks",
			trigger: trigger{Source: triggerRPC},
			progress: []store.Event{
				{EventType: store.EvtPrepareStart},
				{EventType: store.EvtHooksStart, Phase: hooks.CreateAfterEngines.String()},
			},
			wantPhase: store.FinalizeEngines, wantRetry: true, wantRecovery: true,
		},
		{
			name:      "watch re-prepare before its prepare started",
			trigger:   trigger{Source: triggerFileChange},
			wantPhase: store.FinalizeEngines, wantRetry: true, wantRecovery: true,
		},
		{
			name:      "last automatic re-run",
			trigger:   trigger{Source: triggerCreateRetry, Attempt: createRetryAttempts},
			progress:  []store.Event{{EventType: store.EvtHooksStart, Phase: hooks.CreateBeforeEngines.String()}},
			wantPhase: store.FinalizeSetup, wantRetry: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, cleanup := setup(t)
			defer cleanup()
			ctx := context.Background()
			// A config that fails to load turns the engine recovery into an
			// observable worktree:recover:error.
			repo := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte("databases: ["), 0o644); err != nil {
				t.Fatal(err)
			}
			repoID, _ := st.Store.EnsureRepo(ctx, repo, "repo")
			wtID, _ := st.Store.EnsureWorktree(ctx, repoID, filepath.Join(repo, "wt"), "slug", "feature")

			// Progress of an earlier finalize must not count for this one.
			_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareStart, "earlier run", repoID, wtID, "", 0, nil)
			_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateStart, "beginning",
				repoID, wtID, "", 0, map[string]any{"trigger": tc.trigger})
			for _, e := range tc.progress {
				_ = st.Store.WriteEvent(ctx, store.LevelInfo, e.EventType, "step", repoID, wtID, e.Phase, 0, nil)
			}

			SweepStalePreparing(ctx, st)
			st.Store.FlushEvents(ctx)

			failure, setupFailed, err := st.Store.SetupFailure(ctx, wtID)
			if err != nil {
				t.Fatal(err)
			}
			if failure.EventType != store.EvtWorktreeCreateError {
				t.Fatalf("latest lifecycle op = %q, want the restart's create:error", failure.EventType)
			}
			if setupFailed != (tc.wantPhase == store.FinalizeSetup) {
				t.Errorf("setup gate sees a setup failure = %v, want %v (phase %q)", setupFailed, !setupFailed, failure.Phase)
			}

			pending, err := st.Store.PendingCreateRetries(ctx)
			if err != nil {
				t.Fatal(err)
			}
			retried := len(pending) == 1 && pending[0].Retry.Cause == store.RetryDaemonRestart
			if retried != tc.wantRetry {
				t.Errorf("retry pending = %v (%+v), want %v", retried, pending, tc.wantRetry)
			}

			recoveries, err := st.Store.QueryEvents(ctx, store.EventFilter{
				WorktreeID: wtID,
				EventTypes: []string{store.EvtWorktreeRecoverError, store.EvtWorktreeRecoverDrop},
				Limit:      5,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(recoveries) > 0; got != tc.wantRecovery {
				t.Errorf("engine recovery attempted = %v, want %v", got, tc.wantRecovery)
			}
		})
	}
}
