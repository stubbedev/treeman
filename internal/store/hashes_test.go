package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	abs := filepath.Join(dir, name)
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
	return abs
}

func cachedRow(t *testing.T, s *Store, path string) (cachedAt int64, ok bool) {
	t.Helper()
	var size, mtime int64
	var hash string
	row := s.DB.QueryRow(`SELECT size, mtime_ns, hash, cached_at FROM file_hashes WHERE path = ?`, path)
	if err := row.Scan(&size, &mtime, &hash, &cachedAt); err != nil {
		return 0, false
	}
	return cachedAt, true
}

func TestBatchHashedFiles_FreshAndCacheHit(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	dir := t.TempDir()
	a := writeFile(t, dir, "a.txt", "alpha")
	b := writeFile(t, dir, "b.txt", "bravo")

	first, err := s.BatchHashedFiles(ctx, []string{a, b})
	if err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	if first[a] == "" || first[b] == "" || first[a] == first[b] {
		t.Fatalf("unexpected fresh hashes: %+v", first)
	}
	firstCachedAt, ok := cachedRow(t, s, a)
	if !ok {
		t.Fatal("row not persisted")
	}

	// Force a measurable time delta so cached_at refresh is visible.
	time.Sleep(2 * time.Millisecond)

	second, err := s.BatchHashedFiles(ctx, []string{a, b})
	if err != nil {
		t.Fatalf("batch 2: %v", err)
	}
	if second[a] != first[a] || second[b] != first[b] {
		t.Fatalf("hash drift across cache hit: first=%+v second=%+v", first, second)
	}
	secondCachedAt, _ := cachedRow(t, s, a)
	if secondCachedAt <= firstCachedAt {
		t.Errorf("cached_at not refreshed on hit: %d → %d", firstCachedAt, secondCachedAt)
	}
}

func TestBatchHashedFiles_StaleMtimeInvalidates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "m.txt", "v1")
	h1, err := s.BatchHashedFiles(ctx, []string{p})
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite contents + bump mtime to force a miss.
	if err := os.WriteFile(p, []byte("v2-different-length"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	h2, err := s.BatchHashedFiles(ctx, []string{p})
	if err != nil {
		t.Fatal(err)
	}
	if h1[p] == h2[p] {
		t.Fatalf("hash should change after edit: %s vs %s", h1[p], h2[p])
	}
}

func TestBatchHashedFiles_IdenticalContentSameHash(t *testing.T) {
	// Identical bytes at two paths hash to the same digest because
	// BLAKE3 is content-derived — each path is hashed independently
	// (no cross-path reuse). Each gets its own stat-gated row.
	ctx := context.Background()
	s := openTestStore(t)
	root := t.TempDir()
	wtA := filepath.Join(root, "wtA")
	wtB := filepath.Join(root, "wtB")
	for _, d := range []string{wtA, wtB} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pA := writeFile(t, wtA, "composer.lock", "identical bytes")
	pB := writeFile(t, wtB, "composer.lock", "identical bytes")
	stamp := time.Now().Add(-time.Hour)
	for _, p := range []string{pA, pB} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	resA, err := s.BatchHashedFiles(ctx, []string{pA})
	if err != nil {
		t.Fatal(err)
	}
	resB, err := s.BatchHashedFiles(ctx, []string{pB})
	if err != nil {
		t.Fatal(err)
	}
	if resA[pA] == "" || resA[pA] != resB[pB] {
		t.Fatalf("identical content must hash identically: %s vs %s", resA[pA], resB[pB])
	}
	if _, ok := cachedRow(t, s, pA); !ok {
		t.Error("pA row missing after first lookup")
	}
	if _, ok := cachedRow(t, s, pB); !ok {
		t.Error("pB row missing after first lookup")
	}
}

// TestBatchHashedFiles_NoCrossPathAliasing is the macOS-safety
// regression. Two DISTINCT-content files that share an identical
// (size, mtime_ns) — the collision a 1-second-resolution filesystem
// (HFS+, SMB, some FUSE mounts) routinely produces — must hash to
// DIFFERENT digests. The removed content-addressable secondary index
// keyed reuse purely on (size, mtime_ns), so it would have aliased
// these to one hash and let an edited migration keep a stale
// fingerprint. Per-path gating reads each file's own bytes.
func TestBatchHashedFiles_NoCrossPathAliasing(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	root := t.TempDir()

	// Same byte length, different content; pinned to the same mtime.
	pX := writeFile(t, root, "x.sql", "ALTER TABLE a;")
	pY := writeFile(t, root, "y.sql", "ALTER TABLE b;")
	stamp := time.Now().Add(-time.Hour)
	for _, p := range []string{pX, pY} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	// Hash pX first so its (size,mtime,hash) row is persisted. The old
	// secondary index queried persisted rows, so it only aliased across
	// separate calls — hashing both at once on an empty table wouldn't
	// reproduce it. Two calls is the faithful regression.
	resX, err := s.BatchHashedFiles(ctx, []string{pX})
	if err != nil {
		t.Fatal(err)
	}
	resY, err := s.BatchHashedFiles(ctx, []string{pY})
	if err != nil {
		t.Fatal(err)
	}
	if resX[pX] == "" || resY[pY] == "" {
		t.Fatalf("missing hash: x=%q y=%q", resX[pX], resY[pY])
	}
	if resX[pX] == resY[pY] {
		t.Fatalf("distinct content sharing (size,mtime) must not alias: both %s", resX[pX])
	}
}

func TestBatchHashedFiles_SkipMissingAndDirs(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	dir := t.TempDir()
	file := writeFile(t, dir, "x.txt", "x")
	missing := filepath.Join(dir, "no-such-file")

	out, err := s.BatchHashedFiles(ctx, []string{file, missing, dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out[file]; !ok {
		t.Error("regular file dropped")
	}
	if _, ok := out[missing]; ok {
		t.Error("missing file should not appear in result")
	}
	if _, ok := out[dir]; ok {
		t.Error("directory should not be hashed")
	}
}

func TestBatchHashedFiles_RejectsRelativePath(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.BatchHashedFiles(ctx, []string{"relative/path.txt"}); err == nil {
		t.Fatal("expected error for relative path")
	}
}

func TestBatchDirHashes_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	keys := []DirHashKey{
		{Dir: dirA, SpecName: "laravel", HashMode: "filename", MtimeNs: 111, MemberCnt: 3},
		{Dir: dirB, SpecName: "laravel", HashMode: "filename", MtimeNs: 222, MemberCnt: 7},
	}
	hashes := map[string]string{dirA: "hashA", dirB: "hashB"}
	if err := s.UpsertDirHashes(ctx, keys, hashes); err != nil {
		t.Fatal(err)
	}
	got, err := s.BatchDirHashes(ctx, []string{dirA, dirB}, "laravel", "filename")
	if err != nil {
		t.Fatal(err)
	}
	if got[dirA].MemberHash != "hashA" || got[dirA].MemberCount != 3 || got[dirA].MtimeNs != 111 {
		t.Errorf("dirA round-trip wrong: %+v", got[dirA])
	}
	if got[dirB].MemberHash != "hashB" || got[dirB].MemberCount != 7 || got[dirB].MtimeNs != 222 {
		t.Errorf("dirB round-trip wrong: %+v", got[dirB])
	}

	// (spec_name, hash_mode) is part of the key — different mode →
	// no row visible.
	miss, err := s.BatchDirHashes(ctx, []string{dirA}, "laravel", "checksum")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := miss[dirA]; ok {
		t.Error("hash_mode mismatch should not return a row")
	}
}

func TestBatchDirHashes_OverwriteOnConflict(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	dir := t.TempDir()
	first := []DirHashKey{{Dir: dir, SpecName: "rails", HashMode: "filename", MtimeNs: 1, MemberCnt: 1}}
	if err := s.UpsertDirHashes(ctx, first, map[string]string{dir: "old"}); err != nil {
		t.Fatal(err)
	}
	second := []DirHashKey{{Dir: dir, SpecName: "rails", HashMode: "filename", MtimeNs: 999, MemberCnt: 42}}
	if err := s.UpsertDirHashes(ctx, second, map[string]string{dir: "new"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.BatchDirHashes(ctx, []string{dir}, "rails", "filename")
	if err != nil {
		t.Fatal(err)
	}
	if got[dir].MemberHash != "new" || got[dir].MtimeNs != 999 || got[dir].MemberCount != 42 {
		t.Errorf("conflict overwrite failed: %+v", got[dir])
	}
}
