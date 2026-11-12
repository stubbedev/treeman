package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
)

func watchLifecycleEvents(t *testing.T, st *State, wt string) []store.Event {
	t.Helper()
	row, err := st.Store.LookupActiveWorktreeByPath(t.Context(), wt)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := st.Store.QueryEvents(t.Context(), store.EventFilter{
		WorktreeID: row.ID,
		EventTypes: []string{store.EvtWorktreeCreateStart, store.EvtWorktreeCreateEnd, store.EvtWorktreeCreateError},
	})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func newWatchWorktree(t *testing.T, migrate string) (st *State, repo, wt string) {
	t.Helper()
	return newWatchWorktreeWithConfig(
		t,
		"auto_fetch:\n  enabled: false\ndatabases:\n  - engine: sqlite\n    name_template: data/{slug}.db\n    migrate:\n      run: "+migrate+"\n",
	)
}

func newWatchWorktreeWithConfig(t *testing.T, cfg string) (st *State, repo, wt string) {
	t.Helper()
	requireGitAutofetch(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("TREEMAN_CONFIG", "")
	st = newTestState(t)
	repo = filepath.Join(t.TempDir(), "repo")
	gitRun(t, "", "init", "-q", "-b", "main", repo)
	gitRun(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	writeRepoConfig(t, repo, cfg)
	wt = filepath.Join(repo, ".worktrees", "feat")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "feat", wt)
	return st, repo, wt
}

func writeRepoConfig(t *testing.T, repo, cfg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWatchRePrepareMarksWorktreePreparingThenReady(t *testing.T) {
	st, repo, wt := newWatchWorktree(t, "'mkdir -p data && touch \"$TREEMAN_TARGET_DB\"'")
	if err := FinalizeWorktreeForWatch(t.Context(), st, repo, wt, 0, nil); err != nil {
		t.Fatal(err)
	}
	evs := watchLifecycleEvents(t, st, wt)
	if len(evs) != 2 || evs[1].EventType != store.EvtWorktreeCreateStart || evs[0].EventType != store.EvtWorktreeCreateEnd {
		t.Fatalf("watch re-prepare must be bracketed by create:start and create:end, got %+v", evs)
	}
	if !strings.Contains(evs[1].PayloadJSON, string(triggerFileChange)) {
		t.Errorf("create:start must name the file-change trigger, got %s", evs[1].PayloadJSON)
	}
}

func TestWatchRePrepareFailureLeavesWorktreeInError(t *testing.T) {
	st, repo, wt := newWatchWorktree(t, "'exit 1'")
	if err := FinalizeWorktreeForWatch(t.Context(), st, repo, wt, 0, nil); err == nil {
		t.Fatal("a failed migrate must fail the watch re-prepare")
	}
	evs := watchLifecycleEvents(t, st, wt)
	if len(evs) == 0 || evs[0].EventType != store.EvtWorktreeCreateError || evs[0].Level != store.LevelError {
		t.Fatalf("a failed watch re-prepare must leave the worktree in error, not ready, got %+v", evs)
	}
}

func TestWatchRunOptionsPrepareEveryEngineUnlessWorktreeIsReady(t *testing.T) {
	engines := []string{"mysql", "mongodb", "elasticsearch", "s3"}
	type prior struct {
		name string
		ev   store.Event
	}
	notReady := []prior{
		{"failed in engines", store.Event{EventType: store.EvtWorktreeCreateError, Level: store.LevelError}},
		{
			"failed before engines",
			store.Event{EventType: store.EvtWorktreeCreateError, Level: store.LevelError, Phase: store.PhaseBeforeEngines},
		},
		{"failed prepare", store.Event{EventType: store.EvtPrepareError, Level: store.LevelError}},
		{"preparing", store.Event{EventType: store.EvtWorktreeCreateStart, Level: store.LevelInfo}},
		{"deferred", store.Event{EventType: store.EvtWorktreeCreateDeferred, Level: store.LevelInfo}},
	}
	ready := []prior{
		{"ready", store.Event{EventType: store.EvtWorktreeCreateEnd, Level: store.LevelInfo}},
		{"ready with warnings", store.Event{EventType: store.EvtWorktreeCreateEnd, Level: store.LevelWarn}},
		{"no lifecycle event", store.Event{}},
	}
	prepared := func(opts prepare.RunOptions) []string {
		var out []string
		for i, e := range engines {
			if opts.Includes(i) {
				out = append(out, e)
			}
		}
		return out
	}
	for matched, engine := range engines {
		for _, p := range notReady {
			if got := prepared(watchRunOptions(p.ev, matched, len(engines), prepare.RunOptions{})); !slices.Equal(got, engines) {
				t.Errorf("%s worktree, %s input changed: prepared %v, want every engine %v", p.name, engine, got, engines)
			}
		}
		for _, p := range ready {
			if got := prepared(watchRunOptions(p.ev, matched, len(engines), prepare.RunOptions{})); !slices.Equal(got, []string{engine}) {
				t.Errorf("%s worktree, %s input changed: prepared %v, want only %s", p.name, engine, got, engine)
			}
		}
	}
	for _, p := range ready {
		if got := prepared(watchRunOptions(p.ev, -1, len(engines), prepare.RunOptions{})); !slices.Equal(got, engines) {
			t.Errorf("top-level input changed: prepared %v, want every engine %v", got, engines)
		}
	}
}

const twoSQLiteDatabases = "auto_fetch:\n  enabled: false\n" +
	"hooks:\n  create-before-engines:\n    - run: %s\n" +
	"databases:\n" +
	"  - engine: sqlite\n    name_template: data/a_{slug}.db\n    migrate:\n      run: 'mkdir -p data && touch \"$TREEMAN_TARGET_DB\" migrated_a'\n" +
	"  - engine: sqlite\n    name_template: data/b_{slug}.db\n    migrate:\n      run: %s\n"

const migrateB = "'mkdir -p data && touch \"$TREEMAN_TARGET_DB\" migrated_b'"

func TestWatchRePrepareAfterEngineFailurePreparesEveryDatabase(t *testing.T) {
	st, repo, wt := newWatchWorktreeWithConfig(t, fmt.Sprintf(twoSQLiteDatabases, "'true'", "'exit 1'"))
	if err := FinalizeWorktree(t.Context(), st, repo, wt, nil, false); err == nil {
		t.Fatal("the failing migrate of the second database must fail the finalize")
	}
	writeRepoConfig(t, repo, fmt.Sprintf(twoSQLiteDatabases, "'true'", migrateB))

	if err := FinalizeWorktreeForWatch(t.Context(), st, repo, wt, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "migrated_b")); err != nil {
		t.Fatalf("a watch re-prepare of a failed worktree must prepare every database, not only the matched one: %v", err)
	}
	if evs := watchLifecycleEvents(t, st, wt); evs[0].EventType != store.EvtWorktreeCreateEnd {
		t.Fatalf("every database prepared: want the worktree ready, got %+v", evs[0])
	}
}

func TestWatchRePrepareAfterSetupFailureKeepsWorktreeFailed(t *testing.T) {
	st, repo, wt := newWatchWorktreeWithConfig(t, fmt.Sprintf(twoSQLiteDatabases, "'exit 1'", migrateB))
	if err := FinalizeWorktree(t.Context(), st, repo, wt, nil, false); err == nil {
		t.Fatal("the failing create-before-engines hook must fail the finalize")
	}
	before := watchLifecycleEvents(t, st, wt)
	if before[0].EventType != store.EvtWorktreeCreateError || before[0].Phase != store.PhaseBeforeEngines {
		t.Fatalf("a create-before-engines failure must be recorded as failing before the engines, got %+v", before[0])
	}

	if err := FinalizeWorktreeForWatch(t.Context(), st, repo, wt, 0, nil); err != nil {
		t.Fatal(err)
	}
	if after := watchLifecycleEvents(t, st, wt); len(after) != len(before) || after[0].ID != before[0].ID {
		t.Fatalf("a watch re-prepare must not report a worktree whose setup failed as preparing or ready, got %+v", after[0])
	}
	for _, marker := range []string{"migrated_a", "migrated_b"} {
		if _, err := os.Stat(filepath.Join(wt, marker)); err == nil {
			t.Errorf("%s: a watch re-prepare after a setup failure prepared a database", marker)
		}
	}
	row, err := st.Store.LookupActiveWorktreeByPath(t.Context(), wt)
	if err != nil {
		t.Fatal(err)
	}
	skips, err := st.Store.QueryEvents(t.Context(), store.EventFilter{WorktreeID: row.ID, EventTypes: []string{store.EvtWatchSkip}})
	if err != nil {
		t.Fatal(err)
	}
	if len(skips) != 1 || skips[0].Level != store.LevelWarn || !strings.Contains(skips[0].PayloadJSON, "hook exited") {
		t.Fatalf("a skipped watch re-prepare must say why with the failure, got %+v", skips)
	}
}

func TestFinalizeFailureIsRecordedBeforeQueuedWatchRunsReplay(t *testing.T) {
	st, repo, wt := newWatchWorktreeWithConfig(t, fmt.Sprintf(twoSQLiteDatabases, "'exit 1'", migrateB))
	var heldOnError []bool
	var mu sync.Mutex
	st.Store.RegisterEventHook("test", func(ev store.Event) {
		if ev.EventType == store.EvtWorktreeCreateError {
			mu.Lock()
			heldOnError = append(heldOnError, st.IsFinalizeInFlight(wt))
			mu.Unlock()
		}
	})
	if err := FinalizeWorktree(t.Context(), st, repo, wt, nil, false); err == nil {
		t.Fatal("the failing create-before-engines hook must fail the finalize")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(heldOnError, []bool{true}) {
		t.Fatalf("the failure must be recorded while the finalize still holds the worktree, so a queued watch run sees it: %v", heldOnError)
	}
}
