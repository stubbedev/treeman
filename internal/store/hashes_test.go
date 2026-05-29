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

func TestBatchHashedFiles_SecondaryIndexReuse(t *testing.T) {
	// Worktree A and B carry identical content with identical mtime
	// at different absolute paths. The secondary (size, mtime_ns)
	// index lets the second-worktree lookup skip the disk read.
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
		t.Fatalf("expected identical hashes across worktrees: %s vs %s", resA[pA], resB[pB])
	}
	// Both paths must now have their own row so future lookups hit
	// directly without hitting the secondary index.
	if _, ok := cachedRow(t, s, pA); !ok {
		t.Error("pA row missing after first lookup")
	}
	if _, ok := cachedRow(t, s, pB); !ok {
		t.Error("pB row missing — secondary-index path didn't upsert")
	}
}

// TestBatchHashedFiles_SecondaryBatchMultiMiss exercises the batched
// secondary-index path with more than one miss in a single call:
// multiple distinct (size,mtime) pairs, two misses that share one pair
// (dedup), secondary-index hits, and a genuine miss — all resolved
// together rather than one QueryRow per miss.
func TestBatchHashedFiles_SecondaryBatchMultiMiss(t *testing.T) {
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
	stamp := time.Now().Add(-2 * time.Hour)

	// Seed worktree A: three distinct contents → three (size,mtime)
	// rows in file_hashes for the secondary index to find.
	a1 := writeFile(t, wtA, "f1", "alpha content")
	a2 := writeFile(t, wtA, "f2", "beta content is longer")
	a3 := writeFile(t, wtA, "f3", "gamma")
	for _, p := range []string{a1, a2, a3} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	aRes, err := s.BatchHashedFiles(ctx, []string{a1, a2, a3})
	if err != nil {
		t.Fatal(err)
	}

	// Worktree B, all misses on first lookup:
	//   b1, b2     mirror a1, a2 content+mtime → secondary-index hits.
	//   b1dup      duplicates b1 (same pair)    → dedup, same hash.
	//   bNew       fresh content + fresh mtime  → true miss, new hash.
	b1 := writeFile(t, wtB, "f1", "alpha content")
	b1dup := writeFile(t, wtB, "f1copy", "alpha content")
	b2 := writeFile(t, wtB, "f2", "beta content is longer")
	bNew := writeFile(t, wtB, "fnew", "delta brand new content")
	for _, p := range []string{b1, b1dup, b2} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	fresh := time.Now()
	if err := os.Chtimes(bNew, fresh, fresh); err != nil {
		t.Fatal(err)
	}

	bRes, err := s.BatchHashedFiles(ctx, []string{b1, b1dup, b2, bNew})
	if err != nil {
		t.Fatal(err)
	}

	assertSecondaryBatchResults(t, s, aRes, bRes, a1, a2, a3, b1, b1dup, b2, bNew)
}

// assertSecondaryBatchResults checks the secondary-index reuse, dedup,
// true-miss, and direct-row outcomes of the multi-miss batch. Split out
// of TestBatchHashedFiles_SecondaryBatchMultiMiss so the test body stays
// under the cyclomatic-complexity gate.
func assertSecondaryBatchResults(t *testing.T, s *Store, aRes, bRes map[string]string, a1, a2, a3, b1, b1dup, b2, bNew string) {
	t.Helper()
	if bRes[b1] == "" || bRes[b1] != aRes[a1] {
		t.Errorf("b1 should reuse a1 hash via secondary index: %q vs %q", bRes[b1], aRes[a1])
	}
	if bRes[b1dup] != aRes[a1] {
		t.Errorf("b1dup shares b1's pair, want same hash: %q vs %q", bRes[b1dup], aRes[a1])
	}
	if bRes[b2] == "" || bRes[b2] != aRes[a2] {
		t.Errorf("b2 should reuse a2 hash via secondary index: %q vs %q", bRes[b2], aRes[a2])
	}
	if bRes[bNew] == "" {
		t.Error("bNew should get a freshly computed hash")
	}
	if bRes[bNew] == aRes[a1] || bRes[bNew] == aRes[a2] || bRes[bNew] == aRes[a3] {
		t.Errorf("bNew hash unexpectedly collided with a seeded hash: %q", bRes[bNew])
	}
	// Every B path must now own a direct row so future lookups hit by
	// path without touching the secondary index.
	for _, p := range []string{b1, b1dup, b2, bNew} {
		if _, ok := cachedRow(t, s, p); !ok {
			t.Errorf("row missing for %s after batched lookup", p)
		}
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
