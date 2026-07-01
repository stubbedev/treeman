package es

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentCloneSharedSource proves that concurrent clones of the SAME
// source index (the real case: sibling worktrees seeding off the shared live
// `dev_*` parent) all succeed. The fake cluster enforces the ES rule that
// `_clone` 500s unless the source is currently write-blocked — exactly the
// "must be read-only to resize index" failure seen in the field. Without the
// refcount, one clone's deferred unblock would clear the block another clone
// relies on and that clone would 500. Run with -race to also catch the
// process-wide registry racing. Regression for the branch_scoped seed:parent
// clone failure.
func TestConcurrentCloneSharedSource(t *testing.T) {
	var mu sync.Mutex
	blocked := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/_settings"): // PUT block toggle
			idx := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/_settings")
			ro := strings.Contains(readAll(r), `"write":true`)
			mu.Lock()
			blocked[idx] = ro
			mu.Unlock()
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case strings.Contains(path, "/_clone/"):
			src := strings.TrimPrefix(strings.Split(path, "/_clone/")[0], "/")
			mu.Lock()
			ok := blocked[src]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"reason":"index ` + src +
					` must be read-only to resize index. use \"index.blocks.write=true\""},"status":500}`))
				return
			}
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case strings.HasSuffix(path, "/_recovery"):
			_, _ = w.Write([]byte(`{"idx":{"shards":[{"stage":"DONE"}]}}`))
		case strings.HasSuffix(path, "/_alias"):
			_, _ = w.Write([]byte(`{}`)) // no aliases → copyAliases is a no-op
		default:
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		}
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	const src = "dev_TestConcurrentCloneSharedSource_idx" // unique: registry never GCs
	const n = 24

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dstPrefix := fmt.Sprintf("kho_%d_", i)
			dst := dstPrefix + strings.TrimPrefix(src, "dev_")
			errs[i] = d.cloneOneIndex(context.Background(), src, dst, "dev_", dstPrefix)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("clone %d failed: %v", i, err)
		}
	}
	// Last releaser must leave the shared source writable again.
	mu.Lock()
	stillBlocked := blocked[src]
	mu.Unlock()
	if stillBlocked {
		t.Errorf("source %s left read-only after all clones released", src)
	}
}

// TestSourceBlockRefcount asserts the set-once / clear-once semantics
// deterministically, without timing: N acquires flip the block on exactly
// once, and only the final release flips it off.
func TestSourceBlockRefcount(t *testing.T) {
	const idx = "TestSourceBlockRefcount_idx"
	var sets, clears int
	set := func(context.Context) error { sets++; return nil }
	clear := func(context.Context) error { clears++; return nil }
	ctx := context.Background()

	for range 5 {
		if err := acquireSourceReadOnly(ctx, "base", idx, set); err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	if sets != 1 {
		t.Errorf("set called %d times, want 1", sets)
	}
	for i := range 5 {
		releaseSourceReadOnly(ctx, "base", idx, clear)
		want := 0
		if i == 4 {
			want = 1
		}
		if clears != want {
			t.Errorf("after %d releases: clear called %d times, want %d", i+1, clears, want)
		}
	}

	// Acquire failure must not increment: a following release is a no-op.
	const idx2 = "TestSourceBlockRefcount_idx2"
	boom := errors.New("boom")
	if err := acquireSourceReadOnly(ctx, "base", idx2, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("acquire want boom, got %v", err)
	}
	cleared := 0
	releaseSourceReadOnly(ctx, "base", idx2, func(context.Context) error { cleared++; return nil })
	if cleared != 0 {
		t.Errorf("release after failed acquire cleared %d times, want 0", cleared)
	}
}

// TestCloneRetriesWhenSourceBlockCleared proves cloneOneIndex re-asserts the
// write-block and retries when a _clone is rejected because the source lost its
// block mid-clone (external writer / cluster-state lag). The fake cluster
// clears the block right after the first _clone attempt, so the first attempt
// 500s and the retry — which re-asserts the block — succeeds.
func TestCloneRetriesWhenSourceBlockCleared(t *testing.T) {
	var mu sync.Mutex
	blocked := map[string]bool{}
	cloneAttempts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/_settings"):
			idx := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/_settings")
			ro := strings.Contains(readAll(r), `"write":true`)
			mu.Lock()
			blocked[idx] = ro
			mu.Unlock()
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case strings.Contains(path, "/_clone/"):
			src := strings.TrimPrefix(strings.Split(path, "/_clone/")[0], "/")
			mu.Lock()
			cloneAttempts++
			attempt := cloneAttempts
			reasserted := blocked[src]
			mu.Unlock()
			// Attempt 1: the block was cleared out from under us just before ES
			// ran the resize → 500. Retry #2 only succeeds if it re-asserted.
			if attempt == 1 || !reasserted {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"reason":"index ` + src +
					` must be read-only to resize index."},"status":500}`))
				return
			}
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case strings.HasSuffix(path, "/_recovery"):
			_, _ = w.Write([]byte(`{"idx":{"shards":[{"stage":"DONE"}]}}`))
		case strings.HasSuffix(path, "/_alias"):
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		}
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	const src = "dev_TestCloneRetriesWhenSourceBlockCleared_idx"
	// First _clone sees no block (test seeds none) → 500; retry re-asserts the
	// block and the second attempt sees it set → success.
	if err := d.cloneOneIndex(context.Background(), src, "kho_0_"+strings.TrimPrefix(src, "dev_"), "dev_", "kho_0_"); err != nil {
		t.Fatalf("clone should succeed after retry: %v", err)
	}
	mu.Lock()
	attempts := cloneAttempts
	mu.Unlock()
	if attempts < 2 {
		t.Errorf("expected a retry (>=2 clone attempts), got %d", attempts)
	}
}

// TestCloneDoesNotRetryOtherErrors asserts a non-read-only failure fails fast
// (no re-assert loop): the retry is scoped to the block-cleared condition.
func TestCloneDoesNotRetryOtherErrors(t *testing.T) {
	cloneAttempts := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_settings"):
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case strings.Contains(r.URL.Path, "/_clone/"):
			mu.Lock()
			cloneAttempts++
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"reason":"resource_already_exists_exception"},"status":400}`))
		default:
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		}
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	if err := d.cloneOneIndex(context.Background(), "dev_TestCloneDoesNotRetryOtherErrors_idx", "kho_0_x", "dev_", "kho_0_"); err == nil {
		t.Fatal("expected clone to fail")
	}
	mu.Lock()
	attempts := cloneAttempts
	mu.Unlock()
	if attempts != 1 {
		t.Errorf("non-read-only error must not retry: got %d attempts, want 1", attempts)
	}
}

// TestSourceBlockRetiredAfterRelease asserts the registry GCs an entry once
// its count returns to zero — the map stays bounded by in-flight blocks.
func TestSourceBlockRetiredAfterRelease(t *testing.T) {
	const idx = "TestSourceBlockRetiredAfterRelease_idx"
	noop := func(context.Context) error { return nil }
	ctx := context.Background()

	if err := acquireSourceReadOnly(ctx, "base", idx, noop); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	present := func() bool {
		srcBlockRegMu.Lock()
		defer srcBlockRegMu.Unlock()
		_, ok := srcBlockReg[srcBlockKey("base", idx)]
		return ok
	}
	if !present() {
		t.Fatal("entry missing while held")
	}
	releaseSourceReadOnly(ctx, "base", idx, noop)
	if present() {
		t.Errorf("entry not retired after final release")
	}
}

// TestSourceBlockConcurrentChurn hammers acquire/release on one key so the
// retire/resurrect path runs constantly. Under -race it flags any torn access
// to the registry or a ref; the final check proves the count stays balanced
// (no stuck or orphaned entry).
func TestSourceBlockConcurrentChurn(t *testing.T) {
	const idx = "TestSourceBlockConcurrentChurn_idx"
	noop := func(context.Context) error { return nil }
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := acquireSourceReadOnly(ctx, "base", idx, noop); err != nil {
				t.Error(err)
				return
			}
			releaseSourceReadOnly(ctx, "base", idx, noop)
		}()
	}
	wg.Wait()

	srcBlockRegMu.Lock()
	_, present := srcBlockReg[srcBlockKey("base", idx)]
	srcBlockRegMu.Unlock()
	if present {
		t.Errorf("entry leaked after concurrent churn")
	}
}

func readAll(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(r.Body)
	return string(b)
}
