package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
)

func blockedOrphanDropHandler(entered, release chan struct{}, drops *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			if drops.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
			return
		}
		_, _ = w.Write([]byte(`[{"index":"orphan_index"}]`))
	}
}

func TestTeardownOrphanDoesNotLockRepoDuringEngineDrop(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("TREEMAN_CONFIG", "")
	st := newTestState(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var drops atomic.Int32
	server := httptest.NewServer(blockedOrphanDropHandler(entered, release, &drops))
	defer server.Close()
	defer unblock()
	repoPath := t.TempDir()
	wtPath := filepath.Join(repoPath, "gone")
	cfg := fmt.Sprintf(
		"connections:\n  elasticsearch:\n    url: %s\ndatabases:\n  - engine: elasticsearch\n    key_prefix: '{slug}_'\n",
		server.URL,
	)
	if err := os.WriteFile(filepath.Join(repoPath, ".treeman.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, wtPath, "orphan", "feature")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- teardownOrphan(ctx, st, repoPath, wtPath) }()
	defer func() {
		unblock()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("orphan teardown: %v", err)
			}
		case <-ctx.Done():
			t.Error("orphan teardown did not finish")
		}
		if st.IsTeardownInFlight(wtPath) {
			t.Error("orphan remained in flight after teardown")
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("engine drop was not reached")
	}
	mu := st.LockRepoTeardown(repoPath)
	if !mu.TryLock() {
		t.Fatal("orphan engine drop holds the repo mutex needed by unrelated worktree removal")
	}
	mu.Unlock()
	resp := Dispatch(ctx, st, make(chan struct{}, 1), rpc.Request{Method: rpc.MethodDaemonState})
	if resp.State == nil || !slices.Contains(resp.State.InFlightTeardowns, wtPath) {
		t.Fatal("daemon state omits the in-flight orphan reap")
	}
	duplicate := make(chan error, 1)
	go func() { duplicate <- teardownOrphan(ctx, st, repoPath, wtPath) }()
	select {
	case err := <-duplicate:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("duplicate orphan teardown did not return")
	}
	if drops.Load() != 1 {
		t.Fatalf("engine drops = %d, want 1", drops.Load())
	}
	if !st.IsTeardownInFlight(wtPath) {
		t.Fatal("duplicate orphan teardown cleared the original in-flight marker")
	}
	events, err := st.Store.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtDBTeardownProgress}})
	if err != nil || len(events) == 0 {
		t.Fatalf("missing engine drop progress: events=%v err=%v", events, err)
	}
}

func TestTeardownOrphanClearsInFlightOnEarlyReturn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TREEMAN_CONFIG", "")
	for _, tc := range []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{name: "unregistered", cfg: "{}\n"},
		{name: "invalid-config", cfg: "databases: [", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestState(t)
			repoPath := t.TempDir()
			wtPath := filepath.Join(repoPath, "gone")
			if err := os.WriteFile(filepath.Join(repoPath, ".treeman.yaml"), []byte(tc.cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			err := teardownOrphan(t.Context(), st, repoPath, wtPath)
			if (err != nil) != tc.wantErr {
				t.Fatalf("teardown error = %v, wantErr = %v", err, tc.wantErr)
			}
			if st.IsTeardownInFlight(wtPath) {
				t.Fatal("orphan remained in flight after early return")
			}
		})
	}
}
