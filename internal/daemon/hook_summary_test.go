package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

func TestPipelineContinueOnError(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		optional, requiredFailure, skipPrepare bool
	}{
		{name: "default fail fast"},
		{name: "optional permits prepare", optional: true},
		{name: "mixed failures abort", optional: true, requiredFailure: true},
		{name: "skip prepare remains respected", optional: true, skipPrepare: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, cleanup := setup(t)
			defer cleanup()
			ctx := context.Background()
			repo, root := t.TempDir(), t.TempDir()
			repoID, err := st.Store.EnsureRepo(ctx, repo, "repo")
			if err != nil {
				t.Fatal(err)
			}
			wtID, err := st.Store.EnsureWorktree(ctx, repoID, root, "slug", "feature")
			if err != nil {
				t.Fatal(err)
			}
			summary := &hookSummary{}
			ctx = context.WithValue(ctx, hookSummaryKey{}, summary)
			required := "touch deps-ready"
			if tc.requiredFailure {
				required = "exit 9"
			}
			cfg := config.Config{
				Databases: []config.DatabaseConfig{{Engine: "mysql", NameTemplate: "db_{slug}"}},
				Hooks: config.HooksConfig{
					OnCreateBeforeEngines: []config.Action{
						{Run: []string{"echo invalid-lockfile >&2; exit 7"}, ContinueOnError: tc.optional},
						{Run: []string{required}},
					},
					OnCreateAfterEngines: []config.Action{{Run: []string{"test -f deps-ready && touch post-ran"}}},
				},
			}
			touched := false
			done, err := runFinalizeSetupPipeline(
				ctx,
				st,
				&cfg,
				repo,
				root,
				slug.Slug{Value: "slug"},
				false,
				repoID,
				wtID,
				nil,
				tc.skipPrepare,
				&touched,
			)
			wantSuccess := tc.optional && !tc.requiredFailure
			if (err == nil) != wantSuccess || done {
				t.Fatalf("done=%v err=%v", done, err)
			}
			if touched != (wantSuccess && !tc.skipPrepare) {
				t.Fatalf("enginesTouched = %v", touched)
			}
			_, postErr := os.Stat(filepath.Join(root, "post-ran"))
			if (postErr == nil) != wantSuccess {
				t.Fatalf("post hook: %v", postErr)
			}
			failedID := assertFailedHookPersisted(ctx, t, st.Store, wtID)
			if !wantSuccess {
				return
			}
			summary.emit(ctx, st.Store, repoID, wtID)
			assertHookWarningSummary(ctx, t, st.Store, wtID, failedID)
		})
	}
}

func assertFailedHookPersisted(ctx context.Context, t *testing.T, st *store.Store, wtID int64) int64 {
	t.Helper()
	runs, err := st.QueryHookRuns(ctx, wtID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var failedID int64
	for _, run := range runs {
		if run.ExitCode.Valid && run.ExitCode.Int64 == 7 {
			failedID = run.ID
			if !strings.Contains(run.StderrTail, "invalid-lockfile") {
				t.Fatal("failure output lost")
			}
		}
	}
	if failedID == 0 {
		t.Fatal("failed hook was not persisted")
	}
	return failedID
}

func assertHookWarningSummary(ctx context.Context, t *testing.T, st *store.Store, wtID, failedID int64) {
	t.Helper()
	events, err := st.QueryEvents(
		ctx,
		store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtWorktreeCreateEnd}, Limit: 1},
	)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	e := events[0]
	if e.Level != store.LevelWarn || !strings.Contains(e.Message, "non-fatal hook failure") ||
		!strings.Contains(e.Message, "invalid-lockfile") ||
		!strings.Contains(e.Message, "--show") {
		t.Fatalf("summary: %+v", e)
	}
	var payload struct {
		Warnings []hookWarning `json:"non_fatal_hooks"`
	}
	if err := json.Unmarshal([]byte(e.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Warnings) != 1 || payload.Warnings[0].RunID != failedID || payload.Warnings[0].ExitCode != 7 {
		t.Fatalf("payload: %+v", payload)
	}
}

func TestHookSummaryCleanRetry(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()
	repoID, _ := st.Store.EnsureRepo(ctx, t.TempDir(), "repo")
	wtID, _ := st.Store.EnsureWorktree(ctx, repoID, t.TempDir(), "slug", "feature")
	(&hookSummary{warnings: []hookWarning{{Phase: "create-before-engines", ExitCode: 1}}}).emit(ctx, st.Store, repoID, wtID)
	(&hookSummary{}).emit(ctx, st.Store, repoID, wtID)
	events, err := st.Store.QueryEvents(
		ctx,
		store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtWorktreeCreateEnd}, Limit: 1},
	)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if events[0].Level != store.LevelInfo || strings.Contains(events[0].Message, "failure") {
		t.Fatalf("stale warning: %+v", events[0])
	}
}

// The happy path — no non-fatal hook failures — leaves `payload` a
// typed-nil map. Passed to WriteEvent under a run_id-carrying ctx (every
// finalize runs under one) that used to panic inside injectRunID, so the
// worktree:create:end event was never written and `treeman worktree wait`
// sat there until its 10m timeout.
func TestHookSummaryEmitWithoutWarnings(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := runid.With(context.Background(), "run12345")
	repoID, err := st.Store.EnsureRepo(ctx, t.TempDir(), "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, t.TempDir(), "slug", "feature")
	if err != nil {
		t.Fatal(err)
	}

	(&hookSummary{}).emit(ctx, st.Store, repoID, wtID)

	events, err := st.Store.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtWorktreeCreateEnd},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("want exactly one %s event, got %d", store.EvtWorktreeCreateEnd, len(events))
	}
	if events[0].Level != store.LevelInfo {
		t.Errorf("level = %q, want %q", events[0].Level, store.LevelInfo)
	}
	if !strings.Contains(events[0].PayloadJSON, `"run_id":"run12345"`) {
		t.Errorf("run_id missing from payload: %s", events[0].PayloadJSON)
	}
}
