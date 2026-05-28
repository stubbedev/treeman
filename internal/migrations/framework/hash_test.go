package framework

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeCache implements HashCache without touching SQLite. Tracks
// call counts per method so tests can assert the dir-mtime cache
// short-circuits both the file-level cache lookup and the dir-level
// recompute.
type fakeCache struct {
	mu             sync.Mutex
	fileHashes     map[string]string
	dirHashes      map[string]DirHashCacheRecord
	hashedCalls    int
	batchCalls     int
	batchDirCalls  int
	upsertDirCalls int
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		fileHashes: map[string]string{},
		dirHashes:  map[string]DirHashCacheRecord{},
	}
}

func (f *fakeCache) HashedFile(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hashedCalls++
	if h, ok := f.fileHashes[p]; ok {
		return h, nil
	}
	h, err := hashFileBLAKE3(p)
	if err != nil {
		return "", err
	}
	f.fileHashes[p] = h
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

func (f *fakeCache) BatchDirHashes(_ context.Context, dirs []string, _, _ string) (map[string]DirHashCacheRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchDirCalls++
	out := make(map[string]DirHashCacheRecord, len(dirs))
	for _, d := range dirs {
		if r, ok := f.dirHashes[d]; ok {
			out[d] = r
		}
	}
	return out, nil
}

func (f *fakeCache) UpsertDirHashes(_ context.Context, entries []DirHashCacheKey, hashes map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upsertDirCalls++
	for _, e := range entries {
		h, ok := hashes[e.Dir]
		if !ok {
			continue
		}
		f.dirHashes[e.Dir] = DirHashCacheRecord{
			MtimeNs:     e.MtimeNs,
			MemberCount: e.MemberCnt,
			MemberHash:  h,
		}
	}
	return nil
}

// laravelSpec is the smallest Spec needed to exercise the
// dir-mtime gate. Migrations live in a single directory and use
// HashFilename so file bytes are irrelevant.
func laravelSpec(root string) Spec {
	_ = root
	return Spec{
		Name:          "laravel",
		MigrationDirs: []string{"database/migrations"},
		FileGlobs:     []string{"*.php"},
		HashMode:      HashFilename,
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

func TestMigrationsHash_DirMtimeCacheShortCircuit(t *testing.T) {
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
	if cache.upsertDirCalls != 1 {
		t.Errorf("first call should upsert exactly once, got %d", cache.upsertDirCalls)
	}

	// Second call with no fs change: dir-mtime + member count match
	// → no upsert, identical hash.
	h2, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash drift on cache hit: %s vs %s", h1, h2)
	}
	if cache.upsertDirCalls != 1 {
		t.Errorf("cache hit must not upsert, got %d total", cache.upsertDirCalls)
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
	// Bump the directory mtime to mimic a real filesystem write.
	now := time.Now()
	_ = os.Chtimes(dir, now, now)

	h2, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatalf("hash should change after adding a migration: still %s", h1)
	}
	if cache.upsertDirCalls != 2 {
		t.Errorf("new file should trigger second upsert, got %d", cache.upsertDirCalls)
	}
}

func TestMigrationsHash_ChecksumModeReusesFileCache(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "migrations")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"-- 1", "-- 2"} {
		name := []string{"001_up.sql", "002_up.sql"}[i]
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Now().Add(-time.Hour)
	_ = os.Chtimes(dir, stamp, stamp)

	spec := Spec{
		Name:          "sqlx-cli",
		MigrationDirs: []string{"migrations"},
		FileGlobs:     []string{"*.sql"},
		HashMode:      HashChecksum,
	}
	cache := newFakeCache()

	h1, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if cache.batchCalls == 0 {
		t.Error("checksum mode should call BatchHashedFiles on cold path")
	}

	batchBefore := cache.batchCalls
	h2, err := MigrationsHashWithCache(ctx, cache, root, spec)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash drift: %s vs %s", h1, h2)
	}
	if cache.batchCalls != batchBefore {
		t.Errorf("dir-mtime hit should skip BatchHashedFiles entirely: %d → %d", batchBefore, cache.batchCalls)
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

// sqlSpec is a single-dir spec parameterised by hash mode, used by the
// content-edit semantics tests below. nil cache forces a fresh compute
// each call so we measure the hash mode, not the cache.
func sqlSpec(mode HashMode) Spec {
	return Spec{
		Name:          "generic",
		MigrationDirs: []string{"migrations"},
		FileGlobs:     []string{"*.sql"},
		HashMode:      mode,
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

// hash: checksum (default) must detect an in-place content edit to an
// existing file (same name) — this is why lockfiles / seeders use it.
func TestMigrationsHash_ChecksumModeDetectsContentEdit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	f := writeOneMigration(t, root, "-- v1")
	spec := sqlSpec(HashChecksum)

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
		t.Errorf("checksum mode must change hash on a content edit: still %s", h1)
	}
}

// hash: filename must IGNORE an in-place content edit — append-only
// migration dirs (Laravel/Rails/Django) rely on this so re-touching a
// committed migration doesn't force a needless rebuild.
func TestMigrationsHash_FilenameModeIgnoresContentEdit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	f := writeOneMigration(t, root, "-- v1")
	spec := sqlSpec(HashFilename)

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
	if h1 != h2 {
		t.Errorf("filename mode must ignore a content edit: %s != %s", h1, h2)
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
