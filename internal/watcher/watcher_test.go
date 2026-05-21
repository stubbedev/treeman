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

	cfg := config.WatcherConfig{
		DebounceMs: 100,
		Paths: []config.WatcherPath{
			{Glob: "database/migrations/**", On: "rebuild"},
		},
	}
	w, err := New(repoRoot, cfg, dispatch)
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
	if events[0].Mode != ModeRebuild {
		t.Errorf("mode=%q want rebuild", events[0].Mode)
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
	cfg := config.WatcherConfig{
		DebounceMs: 100,
		Paths:      []config.WatcherPath{{Glob: "database/migrations/**", On: "auto"}},
	}
	w, err := New(repoRoot, cfg, dispatch)
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
