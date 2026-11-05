package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

func blockingDropConfig(t *testing.T) (cfg string, entered, release chan struct{}) {
	t.Helper()
	entered = make(chan struct{})
	release = make(chan struct{})
	var drops atomic.Int32
	server := httptest.NewServer(blockedOrphanDropHandler(entered, release, &drops))
	t.Cleanup(server.Close)
	cfg = fmt.Sprintf(
		"auto_fetch:\n  enabled: false\nconnections:\n  elasticsearch:\n    url: %s\ndatabases:\n  - engine: elasticsearch\n    key_prefix: '{slug}_'\n",
		server.URL,
	)
	return cfg, entered, release
}

func waitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was not reached", what)
	}
}

func lifecycleEvents(t *testing.T, st *State, wtID int64, types ...string) []store.Event {
	t.Helper()
	evs, err := st.Store.QueryEvents(t.Context(), store.EventFilter{WorktreeID: wtID, EventTypes: types})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestTeardownStoppedByShutdownKeepsTheCheckoutAndIsResumedAtBoot(t *testing.T) {
	cfg, entered, release := blockingDropConfig(t)
	st, repo, wt := newWatchWorktreeWithConfig(t, cfg)
	repoID, err := st.Store.EnsureRepo(t.Context(), repo, "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.Store.EnsureWorktree(t.Context(), repoID, wt, "feat", "feat")
	if err != nil {
		t.Fatal(err)
	}

	ctx, shutdown := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- TeardownWorktree(ctx, st, repo, wt, false, nil) }()
	waitClosed(t, entered, "engine drop")
	shutdown()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a teardown stopped by shutdown must return the cancellation, got %v", err)
	}

	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("a teardown stopped before the checkout removal must leave the checkout in place: %v", err)
	}
	if out := gitOut(t, repo, "worktree", "list", "--porcelain"); !strings.Contains(out, wt) {
		t.Fatalf("git must still list the worktree whose removal never started:\n%s", out)
	}
	if evs := lifecycleEvents(t, st, wtID, store.EvtWorktreeDeleteError); len(evs) != 0 {
		t.Fatalf("a shutdown is not a teardown failure, got delete:error %+v", evs)
	}
	pending, err := st.Store.ListInterruptedTeardowns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(pending, func(r store.WorktreeRow) bool { return r.ID == wtID }) {
		t.Fatalf("the stopped teardown must stay open for the boot sweep, pending = %+v", pending)
	}

	close(release)
	SweepInterruptedTeardowns(t.Context(), st)
	if pending, err := st.Store.ListInterruptedTeardowns(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("the boot sweep must finish the stopped teardown, pending = %+v, err = %v", pending, err)
	}
	assertRowDeleted(t, st, repoID, wtID, true)
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the resumed teardown must remove the checkout, stat err = %v", err)
	}
	evs := lifecycleEvents(t, st, wtID, store.EvtWorktreeDeleteEnd)
	if len(evs) != 1 {
		t.Fatalf("the resumed teardown must end in delete:end, got %+v", evs)
	}
}

func TestOrphanTeardownStoppedByShutdownRecordsNoFailure(t *testing.T) {
	cfg, entered, release := blockingDropConfig(t)
	defer close(release)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("TREEMAN_CONFIG", "")
	st := newTestState(t)
	repo := t.TempDir()
	writeRepoConfig(t, repo, cfg)
	wt := filepath.Join(repo, "gone")
	repoID, err := st.Store.EnsureRepo(t.Context(), repo, "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.Store.EnsureWorktree(t.Context(), repoID, wt, "orphan", "feature")
	if err != nil {
		t.Fatal(err)
	}

	ctx, shutdown := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- teardownOrphan(ctx, st, repo, wt) }()
	waitClosed(t, entered, "engine drop")
	shutdown()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("an orphan teardown stopped by shutdown must return the cancellation, got %v", err)
	}
	if evs := lifecycleEvents(t, st, wtID, store.EvtWorktreeReapError); len(evs) != 0 {
		t.Fatalf("a shutdown is not a reap failure, got reap:error %+v", evs)
	}
	assertRowDeleted(t, st, repoID, wtID, false)
}

func assertRowDeleted(t *testing.T, st *State, repoID, wtID int64, want bool) {
	t.Helper()
	rows, err := st.Store.ListWorktreesForRepo(t.Context(), repoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == wtID {
			if r.Deleted != want {
				t.Fatalf("worktree #%d deleted = %v, want %v", wtID, r.Deleted, want)
			}
			return
		}
	}
	t.Fatalf("worktree #%d missing from the registry", wtID)
}

func TestWaitTeardownsDrainedReportsWhatIsStillRunning(t *testing.T) {
	st := newTestState(t)
	if !st.MarkTeardownInFlight("/wt") {
		t.Fatal("mark in flight")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if stuck := st.WaitTeardownsDrained(ctx); !slices.Equal(stuck, []string{"/wt"}) {
		t.Fatalf("drain past its deadline must report the running teardown, got %v", stuck)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		st.UnmarkTeardownInFlight("/wt")
	}()
	if stuck := st.WaitTeardownsDrained(t.Context()); len(stuck) != 0 {
		t.Fatalf("drain must return once the teardown finishes, got %v", stuck)
	}
}
