package prepare

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// TestEmitCacheMissWritesEvent locks in the observability contract: the
// snapshot_cache_miss event carries the engine, source DB, fingerprint,
// and the reason ("no_row" / "template_gone") so a user grepping the
// event stream can answer "why didn't this hit the cache?" without
// rerunning prepare with extra logging.
func TestEmitCacheMissWritesEvent(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")

	emitCacheMiss(ctx, s, repoID, wtID, "postgres", "app_db", "fp-abc", cacheMissNoRow)

	evs, err := s.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtSnapshotsCacheMiss},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 cache_miss event, got %d", len(evs))
	}
	e := evs[0]
	if e.Level != store.LevelInfo {
		t.Errorf("level = %q, want info", e.Level)
	}
	for _, want := range []string{"engine=postgres", "source=app_db", "reason=no_row", "fingerprint=fp-abc"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message missing %q: %s", want, e.Message)
		}
	}
}

// TestCacheHitRestoreAndFanoutFoldsSource pins the v2-perf invariant:
// on a cache hit the user-facing source namespace is restored as PART of
// the parallel clone fan-out (not serially in front of it), and a clone
// that happens to equal the source name is not restored twice.
func TestCacheHitRestoreAndFanoutFoldsSource(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")

	var mu sync.Mutex
	var seen []string
	restore := func(_ context.Context, _, target string) error {
		mu.Lock()
		seen = append(seen, target)
		mu.Unlock()
		return nil
	}

	d := config.DatabaseConfig{Engine: "postgres"}
	// `app` is both the source and (deliberately) one of the clone names
	// to exercise dedup.
	clones := []string{"app_t1", "app_t2", "app"}
	if _, err := cacheHitRestoreAndFanout(ctx, s, repoID, wtID, templateExists(true), restore, d,
		"_tm_abc", "app", clones, fanoutBudget{}, "fp123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	sort.Strings(seen)
	mu.Unlock()
	want := []string{"app", "app_t1", "app_t2"}
	if len(seen) != len(want) {
		t.Fatalf("restored targets = %v, want %v (source folded in, deduped)", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("restored targets = %v, want %v", seen, want)
		}
	}
}

func templateExists(alive bool) func(context.Context, string) (bool, error) {
	return func(context.Context, string) (bool, error) { return alive, nil }
}

func restoreFailingSource(boom error) cloneRestorer {
	return func(_ context.Context, _, target string) error {
		if target == "app" {
			return boom
		}
		return nil
	}
}

func TestCacheHitRestoreAndFanoutFallbackDropsRowWhenTemplateGone(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")

	const fp = "fp-fallback"
	if err := s.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint: fp, Engine: "postgres", SourceDB: "app",
		TemplateName: "_tm_abc", RepoID: repoID,
	}); err != nil {
		t.Fatal(err)
	}

	d := config.DatabaseConfig{Engine: "postgres"}
	gone, err := cacheHitRestoreAndFanout(ctx, s, repoID, wtID, templateExists(false),
		restoreFailingSource(errors.New("Unknown database '_tm_abc'")), d, "_tm_abc", "app", []string{"app_t1"}, fanoutBudget{}, fp)
	if err != nil || !gone {
		t.Fatalf("a vanished template must report templateGone for a rebuild: gone=%v err=%v", gone, err)
	}
	rec, _ := s.LookupSnapshot(ctx, fp)
	if rec != nil {
		t.Fatal("snapshot row should be deleted on fallback so the engine rebuilds")
	}
}

func TestCacheHitRestoreAndFanoutKeepsLiveTemplate(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")

	const fp = "fp-live"
	if err := s.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint: fp, Engine: "mysql", SourceDB: "app",
		TemplateName: "_tm_abc", RepoID: repoID,
	}); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("pin mysql connection: Error 1040: Too many connections")
	d := config.DatabaseConfig{Engine: "mysql"}
	gone, err := cacheHitRestoreAndFanout(ctx, s, repoID, wtID, templateExists(true),
		restoreFailingSource(boom), d, "_tm_abc", "app", []string{"app_t1"}, fanoutBudget{}, fp)
	if gone || !errors.Is(err, boom) {
		t.Fatalf("a restore failure against a live template must surface, not rebuild: gone=%v err=%v", gone, err)
	}
	if rec, _ := s.LookupSnapshot(ctx, fp); rec == nil {
		t.Fatal("a live template's snapshot row must survive a failed restore")
	}
	evs, err := s.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtSnapshotsCacheFallback}})
	if err != nil || len(evs) != 0 {
		t.Fatalf("no rebuild fallback may be announced for a live template: %v %v", evs, err)
	}
}
