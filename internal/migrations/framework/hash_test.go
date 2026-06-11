package framework

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeCache implements HashCache without touching SQLite. Tracks call
// counts so tests can assert the content-hash path re-reads a file
// only when its own (size,mtime) changed — and that the fingerprint
// is recomputed from member content on every call, with no dir-level
// short-circuit.
type fakeCache struct {
	mu          sync.Mutex
	fileCache   map[string]cachedFile
	hashedCalls int
	batchCalls  int
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		fileCache: map[string]cachedFile{},
	}
}

// cachedFile is one stat-gated row: the hash plus the (size,mtime)
// it was computed against. Mirrors the real store's file_hashes gate.
type cachedFile struct {
	size, mtime int64
	hash        string
}

// HashedFile returns p's content hash, re-reading from disk only when
// p's own (size,mtime) differs from the cached row — exactly the
// store's per-path content-addressed gate. hashedCalls counts the
// disk reads so tests can prove an unchanged file is served from cache
// and an edited file is re-read.
func (f *fakeCache) HashedFile(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	size, mtime := info.Size(), info.ModTime().UnixNano()
	if c, ok := f.fileCache[p]; ok && c.size == size && c.mtime == mtime {
		return c.hash, nil
	}
	f.hashedCalls++
	h, err := hashFileBLAKE3(p)
	if err != nil {
		return "", err
	}
	f.fileCache[p] = cachedFile{size: size, mtime: mtime, hash: h}
	return h, nil
}

func (f *fakeCache) BatchHashedFiles(ctx context.Context, paths []string) (map[string]string, error) {
	f.mu.Lock()
	f.batchCalls++
	f.mu.Unlock()
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		h, err := f.HashedFile(ctx, p)
		if err != nil {
			continue
		}
		out[p] = h
	}
	return out, nil
}

// laravelSpec is the smallest Spec needed to exercise the dir-mtime
// gate: a single directory of migrations content-hashed.
func laravelSpec(root string) Spec {
	_ = root
	return Spec{
		Name:          "laravel",
		MigrationDirs: []string{"database/migrations"},
		FileGlobs:     []string{"*.php"},
	}
}

func setupLaravelRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "database", "migrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"2024_01_01_000001_init.php", "2024_01_02_000002_users.php"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("<?php"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Stable mtime so test runs are deterministic.
	stamp := time.Now().Add(-time.Hour)
	_ = os.Chtimes(dir, stamp, stamp)
	return root
}

func TestMigrationsHash_StableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	root := setupLaravelRoot(t)
	spec := laravelSpec(root)
	cache := newFakeCache()

	h1, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("empty hash")
	}

	// Second call with no fs change: identical hash, and the file
	// cache serves every member from its stat-gated row — no re-read.
	readsBefore := cache.hashedCalls
	h2, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash drift on unchanged tree: %s vs %s", h1, h2)
	}
	if cache.hashedCalls != readsBefore {
		t.Errorf("unchanged files must be served from cache, got %d new reads", cache.hashedCalls-readsBefore)
	}
}

func TestMigrationsHash_NewMigrationFileBumpsHash(t *testing.T) {
	ctx := context.Background()
	root := setupLaravelRoot(t)
	spec := laravelSpec(root)
	cache := newFakeCache()

	h1, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root, "database", "migrations")
	if err := os.WriteFile(filepath.Join(dir, "2024_01_03_000003_posts.php"), []byte("<?php"), 0o644); err != nil {
		t.Fatal(err)
	}

	h2, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatalf("hash should change after adding a migration: still %s", h1)
	}
}

// An in-place content edit must flip the hash EVEN WITH a cache wired
// — this is the regression for the dir-mtime short-circuit, which
// reused a stale per-dir digest because an edit doesn't bump the
// directory's mtime. The fakeCache is stat-gated like the real store,
// so a passing result proves correctness end-to-end, not just on the
// nil-cache path.
func TestMigrationsHash_DetectsContentEditWithCache(t *testing.T) {
	ctx := context.Background()
	root := setupLaravelRoot(t)
	spec := laravelSpec(root)
	cache := newFakeCache()

	h1, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}

	// Edit an existing migration in place. Bump only the FILE mtime —
	// the directory mtime stays put, exactly as a real editor save
	// leaves it. The old dir-mtime gate would short-circuit here.
	dir := filepath.Join(root, "database", "migrations")
	edited := filepath.Join(dir, "2024_01_01_000001_init.php")
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	dirMtime := dirInfo.ModTime()
	if err := os.WriteFile(edited, []byte("<?php // changed body"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Restore the directory mtime to prove detection does not depend
	// on it (an in-place edit doesn't change it on Linux or macOS).
	if err := os.Chtimes(dir, dirMtime, dirMtime); err != nil {
		t.Fatal(err)
	}

	h2, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatalf("in-place edit must flip the hash even with a cache: still %s", h1)
	}
}

func TestMigrationsHash_NoCacheStillWorks(t *testing.T) {
	ctx := context.Background()
	root := setupLaravelRoot(t)
	spec := laravelSpec(root)
	h, err := MigrationsHashWithCache(ctx, nil, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h == "" {
		t.Fatal("nil-cache path returned empty hash")
	}
}

// sqlSpec is a single-dir spec used by the content-edit semantics
// tests below. nil cache forces a fresh compute each call so we
// measure the hashing path, not the cache.
func sqlSpec() Spec {
	return Spec{
		Name:          "generic",
		MigrationDirs: []string{"migrations"},
		FileGlobs:     []string{"*.sql"},
	}
}

func writeOneMigration(t *testing.T, root, body string) string {
	t.Helper()
	dir := filepath.Join(root, "migrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "001_up.sql")
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// MigrationsHash must detect an in-place content edit on every input
// — there is no longer a `filename` mode that ignored bytes. Pins the
// content-only invariant: editing an existing migration always flips
// the fingerprint.
func TestMigrationsHash_DetectsContentEdit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	f := writeOneMigration(t, root, "-- v1")
	spec := sqlSpec()

	h1, err := MigrationsHashWithCache(ctx, nil, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("-- v2 changed contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := MigrationsHashWithCache(ctx, nil, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Errorf("content-only hashing must flip the hash on an in-place edit: still %s", h1)
	}
}

func TestEnumerateMigrations_OrderedAndFiltered(t *testing.T) {
	root := setupLaravelRoot(t)
	dir := filepath.Join(root, "database", "migrations")
	// Add a non-matching file and a subdirectory to ensure both are
	// excluded from enumeration.
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignore"), 0o644)
	_ = os.Mkdir(filepath.Join(dir, "nested"), 0o755)

	spec := laravelSpec(root)
	got, err := EnumerateMigrations(root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 php files, got %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("not sorted: %v", got)
		}
	}
	for _, p := range got {
		if filepath.Ext(p) != ".php" {
			t.Errorf("non-php leaked in: %s", p)
		}
	}
}
