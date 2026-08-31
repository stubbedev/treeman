package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPrepareCallSitesHoldWorktreeLock is a source invariant: every
// engine-prepare entry point in this package must run under the
// per-worktree prepare lock. Issue #28 was four independent callers
// (finalize pipeline, watcher re-prepare, `prepare` task, `db reset`)
// building the same source databases concurrently — one dump-loading
// while another created tables in the same DB and then migrated against
// a schema whose framework ledger was not written yet.
//
// A source scan rather than a behavioural test because reproducing the
// collision needs a live engine; what regresses in practice is a NEW
// call site added without the lock, which this catches at compile-test
// time.
func TestPrepareCallSitesHoldWorktreeLock(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Split on top-level func declarations so each chunk is one
		// function body (plus its doc comment).
		for fn := range strings.SplitSeq(string(body), "\nfunc ") {
			if !strings.Contains(fn, "prepare.Run(") && !strings.Contains(fn, "prepare.RunFiltered(") {
				continue
			}
			// Skip comment-only mentions: a chunk that names prepare.Run
			// but never calls it outside a comment line.
			if !callsPrepare(fn) {
				continue
			}
			found++
			if !strings.Contains(fn, "LockWorktreePrepare") {
				name, _, _ := strings.Cut(fn, "(")
				t.Errorf("%s: func %s calls prepare.Run/RunFiltered without LockWorktreePrepare — "+
					"concurrent builds would collide on the same source databases (issue #28)", f, name)
			}
		}
	}
	if found == 0 {
		t.Fatal("no prepare.Run call sites found — scan is broken, not clean")
	}
}

// callsPrepare reports whether any non-comment line in the chunk calls
// prepare.Run / prepare.RunFiltered.
func callsPrepare(chunk string) bool {
	for line := range strings.SplitSeq(chunk, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "prepare.Run(") || strings.Contains(trimmed, "prepare.RunFiltered(") {
			return true
		}
	}
	return false
}

// TestLockWorktreePrepareScope — the lock is per worktree: the same path
// serialises, two different paths don't.
func TestLockWorktreePrepareScope(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	if got := st.LockWorktreePrepare("/wt/a"); got != st.LockWorktreePrepare("/wt/a") {
		t.Error("same worktree path returned two different mutexes")
	}
	if st.LockWorktreePrepare("/wt/a") == st.LockWorktreePrepare("/wt/b") {
		t.Error("different worktree paths share a mutex — parallel builds would serialise")
	}

	mu := st.LockWorktreePrepare("/wt/a")
	mu.Lock()
	released := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		second := st.LockWorktreePrepare("/wt/a")
		second.Lock()
		defer second.Unlock()
		close(released)
	})
	select {
	case <-released:
		t.Fatal("second Lock() on the same path succeeded while the first was held")
	case <-time.After(50 * time.Millisecond):
	}
	mu.Unlock()
	wg.Wait()
}
