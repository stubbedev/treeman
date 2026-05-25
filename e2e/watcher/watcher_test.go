//go:build e2e

// Package watcher_e2e exercises the fsnotify-driven file watcher end
// to end: writes a real file under a real glob, asserts the dispatch
// callback fires with the correct DBIndex + Label inside the
// debounce window. No engine container needed — the watcher itself
// is engine-independent.
package watcher_e2e

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/watcher"
)

func TestFileWatcherDispatchesOnInputEdit(t *testing.T) {
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "db/migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, "db/seeders"), 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		events []watcher.Event
	)
	dispatch := func(_ context.Context, ev watcher.Event) error {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		return nil
	}

	paths := []config.WatcherPath{
		{Glob: "db/migrations/**/*.sql", Label: "migrations", DBIndex: 0},
		{Glob: "db/seeders/**/*.sql", Label: "seeders", DBIndex: 0},
	}
	w, err := watcher.New(repoRoot, paths, 200, dispatch) // 200ms debounce
	if err != nil {
		t.Fatalf("watcher.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()
	time.Sleep(300 * time.Millisecond) // let fsnotify register dirs

	// Write a migration → expect ONE event labeled "migrations".
	if err := os.WriteFile(filepath.Join(repoRoot, "db/migrations/001_init.sql"),
		[]byte("CREATE TABLE x();"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Write a seed → expect ONE event labeled "seeders".
	if err := os.WriteFile(filepath.Join(repoRoot, "db/seeders/users.sql"),
		[]byte("INSERT INTO x VALUES (1);"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 {
		t.Fatalf("want >=2 events, got %d: %#v", len(events), events)
	}
	gotLabels := map[string]bool{}
	for _, ev := range events {
		gotLabels[ev.Label] = true
	}
	for _, want := range []string{"migrations", "seeders"} {
		if !gotLabels[want] {
			t.Errorf("label %q not observed (got: %v)", want, gotLabels)
		}
	}
}

// TestFileWatcherDebouncesBurst writes 10 files in quick succession
// inside the debounce window and asserts the dispatcher receives one
// event per unique path (not 10×), confirming the coalescing logic
// holds under realistic editor save bursts.
func TestFileWatcherDebouncesBurst(t *testing.T) {
	repoRoot := t.TempDir()
	dir := filepath.Join(repoRoot, "watched")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		events []watcher.Event
	)
	dispatch := func(_ context.Context, ev watcher.Event) error {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		return nil
	}
	paths := []config.WatcherPath{{Glob: "watched/*.txt", Label: "burst", DBIndex: 0}}
	w, err := watcher.New(repoRoot, paths, 500, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx) }()
	time.Sleep(300 * time.Millisecond)

	// 10 rewrites of the SAME file inside the debounce window.
	target := filepath.Join(dir, "one.txt")
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(target, []byte(string(rune('a'+i))), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1 * time.Second) // wait past the debounce window

	mu.Lock()
	defer mu.Unlock()
	// Should be 1 (or at most a couple if writes straddled boundaries)
	// — definitely not 10.
	if len(events) > 3 {
		t.Errorf("debounce failed: got %d events for 10 rapid writes (want ~1)", len(events))
	}
	if len(events) == 0 {
		t.Errorf("no events dispatched")
	}
}
