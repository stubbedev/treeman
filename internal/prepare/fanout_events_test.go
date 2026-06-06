package prepare

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

// TestFanOutClonesEmitsLifecycleEvents covers the happy path: a
// single fanout_start, one clone_restore_done per clone (debug), and
// one info-level fanout_done. The slowest_db payload should match a
// clone we actually restored.
func TestFanOutClonesEmitsLifecycleEvents(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")

	restore := func(ctx context.Context, template, target string) error { return nil }
	clones := []string{"a", "b", "c"}
	if err := fanOutClones(ctx, s, repoID, wtID, restore, "tmpl", clones, "postgres", 0, 0); err != nil {
		t.Fatal(err)
	}

	all, err := s.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range all {
		counts[e.EventType]++
	}
	if counts["clones:start"] != 1 {
		t.Errorf("fanout_start count = %d, want 1", counts["clones:start"])
	}
	if counts["clones:end"] != 1 {
		t.Errorf("fanout_done count = %d, want 1", counts["clones:end"])
	}
	if counts["clones:restore:end"] != len(clones) {
		t.Errorf("clone_restore_done = %d, want %d", counts["clones:restore:end"], len(clones))
	}
	// Sanity: the per-clone events should sit at debug level so they
	// don't pollute the default info-only tail stream.
	for _, e := range all {
		if e.EventType == "clones:restore:end" && e.Level != store.LevelDebug {
			t.Errorf("clone_restore_done emitted at level %q, want debug", e.Level)
		}
	}
}

// TestFanOutClonesEmitsErrorEventOnFailure — when any clone restore
// returns an error, fanout_done lands at error level and there's a
// clone_restore_fail at warn for the bad target. The first error is
// still surfaced to the caller.
func TestFanOutClonesEmitsErrorEventOnFailure(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/y", "y", "main")

	want := errors.New("boom")
	restore := func(ctx context.Context, template, target string) error {
		if target == "bad" {
			return want
		}
		return nil
	}
	gErr := fanOutClones(ctx, s, repoID, wtID, restore, "tmpl",
		[]string{"good", "bad", "also-good"}, "postgres", 0, 0)
	if gErr == nil || !strings.Contains(gErr.Error(), "boom") {
		t.Fatalf("expected boom-wrapped error, got %v", gErr)
	}

	all, err := s.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID})
	if err != nil {
		t.Fatal(err)
	}
	var done store.Event
	var sawFailEvent bool
	for _, e := range all {
		if e.EventType == "clones:end" {
			done = e
		}
		if e.EventType == "clones:restore:error" {
			sawFailEvent = true
		}
	}
	if !sawFailEvent {
		t.Error("expected at least one clone_restore_fail event")
	}
	if done.EventType == "" {
		t.Fatal("missing fanout_done event")
	}
	if done.Level != store.LevelError {
		t.Errorf("fanout_done level = %q, want error", done.Level)
	}
}
