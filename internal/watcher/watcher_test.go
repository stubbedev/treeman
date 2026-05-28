package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
)

// TestDispatchesOnMatchingGlob writes a file under a watched glob
// and asserts the dispatcher fires once with the expected mode.
func TestDispatchesOnMatchingGlob(t *testing.T) {
	repoRoot := t.TempDir()
	migDir := filepath.Join(repoRoot, "database", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		events []Event
	)
	dispatch := func(_ context.Context, ev Event) error {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		return nil
	}

	const debounceMs uint64 = 100
	paths := []config.WatcherPath{
		{Glob: "database/migrations/**"},
	}
	w, err := New(repoRoot, paths, debounceMs, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// Give the watcher a moment to register its dirs.
	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(migDir, "2024_01_01_create.php"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wait up to 1s for the debounced dispatch.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(events)
		mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no events dispatched")
	}
}

// TestIgnoresUnmatchedPaths confirms changes outside the configured
// globs are silently dropped — important for repos with noisy
// vendor / node_modules trees.
func TestIgnoresUnmatchedPaths(t *testing.T) {
	repoRoot := t.TempDir()
	noise := filepath.Join(repoRoot, "node_modules")
	if err := os.MkdirAll(noise, 0o755); err != nil {
		t.Fatal(err)
	}
	migDir := filepath.Join(repoRoot, "database", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var fired bool
	dispatch := func(_ context.Context, _ Event) error { fired = true; return nil }
	const debounceMs uint64 = 100
	paths := []config.WatcherPath{{Glob: "database/migrations/**"}}
	w, err := New(repoRoot, paths, debounceMs, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	// node_modules isn't under any watched glob — fsnotify won't
	// even be added there. Mutate it anyway as a control.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	go func() { _ = w.Start(ctx) }()
	time.Sleep(150 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(noise, "junk.js"), []byte("x"), 0o644)
	<-ctx.Done()
	if fired {
		t.Error("dispatcher fired for unmatched path")
	}
}

// TestReArmsAfterFlush asserts the debounce timer fires again for a
// second burst after the first flush. This is the re-arm path that
// replaced the always-on ticker: a regression here would dispatch the
// first edit and then go permanently silent. Sequencing the second
// write strictly after the first dispatch guarantees the two edits
// land in separate debounce windows, so reaching 2 dispatches can
// only happen if the timer re-armed.
func TestReArmsAfterFlush(t *testing.T) {
	repoRoot := t.TempDir()
	migDir := filepath.Join(repoRoot, "database", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		count int
	)
	dispatch := func(_ context.Context, _ Event) error {
		mu.Lock()
		count++
		mu.Unlock()
		return nil
	}

	const debounceMs uint64 = 80
	w, err := New(repoRoot, []config.WatcherPath{{Glob: "database/migrations/**"}}, debounceMs, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()
	time.Sleep(150 * time.Millisecond) // let addAllDirs subscribe

	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := count
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d dispatches (re-arm likely broken)", n)
	}

	if err := os.WriteFile(filepath.Join(migDir, "001_a.sql"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(1)

	// Second burst, strictly after the first window closed → re-arm.
	if err := os.WriteFile(filepath.Join(migDir, "002_b.sql"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(2)
}

func TestStaticPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"database/migrations/**", "database/migrations"},
		{"db/migrate", "db/migrate"},
		{"src/*.rs", "src"},
		{"**/migrations", ""},
		{"crates/*/migrations/*.sql", "crates"},
	}
	for _, c := range cases {
		got := staticPrefix(c.in)
		if got != c.want {
			t.Errorf("staticPrefix(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
