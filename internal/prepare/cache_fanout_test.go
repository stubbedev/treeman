package prepare

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

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
	if err := cacheHitRestoreAndFanout(ctx, s, repoID, wtID, restore, d,
		"_tm_abc", "app", clones, 0, "fp123"); err != nil {
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

// TestCacheHitRestoreAndFanoutFallbackDropsRow verifies the merged
// fallback path: any restore failure drops the suspect snapshot row and
// surfaces the error so the engine cold-builds.
func TestCacheHitRestoreAndFanoutFallbackDropsRow(t *testing.T) {
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

	boom := errors.New("template vanished")
	restore := func(_ context.Context, _, target string) error {
		if target == "app" {
			return boom
		}
		return nil
	}

	d := config.DatabaseConfig{Engine: "postgres"}
	err = cacheHitRestoreAndFanout(ctx, s, repoID, wtID, restore, d,
		"_tm_abc", "app", []string{"app_t1"}, 0, fp)
	if err == nil {
		t.Fatal("expected an error from the failed restore")
	}
	rec, _ := s.LookupSnapshot(ctx, fp)
	if rec != nil {
		t.Fatal("snapshot row should be deleted on fallback so the engine cold-builds")
	}
}
