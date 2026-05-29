package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

// TestLockRepoSerialisesConcurrentReloads verifies the per-repo
// mutex added to ConfigReloader prevents two reloadOne goroutines
// from interleaving on the same repo. Uses lockRepo directly since
// the full reloadOne path needs a real git repo + fsnotify; the
// mutex itself is the contract we care about.
func TestLockRepoSerialisesConcurrentReloads(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)
	cr, err := NewConfigReloader(st)
	if err != nil {
		t.Fatal(err)
	}

	var active atomic.Int32
	var maxActive atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			mu := cr.lockRepo("/repos/x")
			mu.Lock()
			defer mu.Unlock()
			n := active.Add(1)
			defer active.Add(-1)
			if n > maxActive.Load() {
				maxActive.Store(n)
			}
			time.Sleep(5 * time.Millisecond)
		})
	}
	wg.Wait()

	if got := maxActive.Load(); got != 1 {
		t.Errorf("max concurrent holders for /repos/x = %d, want 1", got)
	}
}

// TestLockRepoDifferentReposParallelise confirms the per-repo mutex
// only blocks reloads of the SAME repo. Reloads of different repos
// must still run in parallel; otherwise a busy ReloadAll sweep
// serialises every repo's reload behind every other repo's.
func TestLockRepoDifferentReposParallelise(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)
	cr, err := NewConfigReloader(st)
	if err != nil {
		t.Fatal(err)
	}

	muA := cr.lockRepo("/repos/a")
	muB := cr.lockRepo("/repos/b")
	if muA == muB {
		t.Fatalf("different repos got the same mutex (memory aliasing)")
	}
	muA.Lock()
	defer muA.Unlock()
	// muB should be acquirable while muA is held.
	done := make(chan struct{})
	go func() {
		// muB must be acquirable while muA is held — independent
		// per-repo mutexes don't block each other.
		if muB.TryLock() {
			muB.Unlock()
			close(done)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("muB blocked behind muA — per-repo mutexes are not independent")
	}
}
