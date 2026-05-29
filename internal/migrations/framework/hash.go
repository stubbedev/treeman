package framework

import (
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"lukechampine.com/blake3"
)

// HashCache is the subset of *store.Store that MigrationsHashWithCache
// needs. Defined here so the framework package doesn't depend on
// internal/store.
type HashCache interface {
	HashedFile(ctx context.Context, path string) (string, error)
	BatchHashedFiles(ctx context.Context, paths []string) (map[string]string, error)
	BatchDirHashes(ctx context.Context, dirs []string, specName, hashMode string) (map[string]DirHashCacheRecord, error)
	UpsertDirHashes(ctx context.Context, entries []DirHashCacheKey, hashes map[string]string) error
}

// DirHashCacheRecord mirrors store.DirHashRecord. Re-declared here to
// keep framework decoupled from internal/store.
type DirHashCacheRecord struct {
	MtimeNs     int64
	MemberCount int64
	MemberHash  string
}

// DirHashCacheKey mirrors store.DirHashKey.
type DirHashCacheKey struct {
	Dir       string
	SpecName  string
	HashMode  string
	MtimeNs   int64
	MemberCnt int64
}

// MigrationsHash fingerprints the set of migration files claimed by
// `spec` under `repoRoot`. Wraps MigrationsHashWithCache with no
// cache — kept for callers that don't have a Store handle.
func MigrationsHash(repoRoot string, spec Spec) (string, error) {
	return MigrationsHashWithCache(context.Background(), nil, repoRoot, spec)
}

// MigrationsHashWithCache fingerprints the migration set keyed by
// `spec`. The output is the hex BLAKE3-256 of, per migration
// directory in sorted order:
//
//	"<rel-dir>\0" + <per-dir digest> + "\n"
//
// where <per-dir digest> is the hex BLAKE3-256 of, per file in that
// directory in sorted order by basename:
//
//	"<basename>\0" + (HashMode=filename ? "" : "<blake3-of-bytes>") + "\n"
//
// The per-dir digest is cached in `dir_hashes` keyed on absolute
// dir path + (spec name, hash mode). A live (mtime_ns, member_count)
// stat match makes the per-dir contribution reusable without
// re-reading the directory or touching individual files. New /
// changed / deleted migrations bump the dir mtime, invalidating the
// cache automatically.
func MigrationsHashWithCache(ctx context.Context, cache HashCache, repoRoot string, spec Spec) (string, error) {
	dirs, err := resolveMigrationDirs(repoRoot, spec)
	if err != nil {
		return "", err
	}

	type dirEntry struct {
		absDir string
		relDir string
		mtime  int64
	}
	dirRecords := make([]dirEntry, 0, len(dirs))
	statDirs := make([]string, 0, len(dirs))
	for _, abs := range dirs {
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			continue
		}
		rel, err := filepath.Rel(repoRoot, abs)
		if err != nil {
			continue
		}
		dirRecords = append(dirRecords, dirEntry{
			absDir: abs,
			relDir: filepath.ToSlash(rel),
			mtime:  info.ModTime().UnixNano(),
		})
		statDirs = append(statDirs, abs)
	}
	sort.SliceStable(dirRecords, func(i, j int) bool { return dirRecords[i].relDir < dirRecords[j].relDir })

	// Cache lookup for whole directories first. mtime + member count
	// gating means new/renamed/deleted files invalidate the cached
	// digest without a per-file check.
	var cached map[string]DirHashCacheRecord
	if cache != nil {
		cached, _ = cache.BatchDirHashes(ctx, statDirs, spec.Name, string(spec.HashMode))
	}

	freshHashes := make(map[string]string)
	freshKeys := make([]DirHashCacheKey, 0)
	perDirDigest := make(map[string]string, len(dirRecords))

	for i := range dirRecords {
		d := &dirRecords[i]
		entries, err := os.ReadDir(d.absDir)
		if err != nil {
			continue
		}
		matches := filterMigrationEntries(entries, spec)
		memberCount := int64(len(matches))

		if rec, ok := cached[d.absDir]; ok && rec.MtimeNs == d.mtime && rec.MemberCount == memberCount {
			perDirDigest[d.relDir] = rec.MemberHash
			continue
		}

		digest, err := computeDirDigest(ctx, cache, d.absDir, matches, spec)
		if err != nil {
			return "", err
		}
		perDirDigest[d.relDir] = digest
		freshHashes[d.absDir] = digest
		freshKeys = append(freshKeys, DirHashCacheKey{
			Dir:       d.absDir,
			SpecName:  spec.Name,
			HashMode:  string(spec.HashMode),
			MtimeNs:   d.mtime,
			MemberCnt: memberCount,
		})
	}

	if cache != nil && len(freshKeys) > 0 {
		_ = cache.UpsertDirHashes(ctx, freshKeys, freshHashes)
	}

	// Fold per-dir digests into the outer hash in sorted rel-dir order.
	h := blake3.New(32, nil)
	for _, d := range dirRecords {
		digest, ok := perDirDigest[d.relDir]
		if !ok {
			continue
		}
		_, _ = h.Write([]byte(d.relDir))
		_, _ = h.Write([]byte{0})
		if raw, err := hex.DecodeString(digest); err == nil {
			_, _ = h.Write(raw)
		}
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// computeDirDigest produces the per-directory digest. HashChecksum
// hashes file bytes (cache-assisted via BatchHashedFiles when
// available); HashFilename skips bytes entirely.
func computeDirDigest(ctx context.Context, cache HashCache, absDir string, matches []os.DirEntry, spec Spec) (string, error) {
	if len(matches) == 0 {
		// Stable empty-dir digest so an empty migrations folder
		// contributes a constant per-dir hash.
		h := blake3.New(32, nil)
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Name() < matches[j].Name() })

	if spec.HashMode != HashChecksum {
		h := blake3.New(32, nil)
		for _, e := range matches {
			_, _ = h.Write([]byte(e.Name()))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte{'\n'})
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	// HashChecksum: collect absolute paths, batch-lookup hashes
	// (single SELECT IN + parallel-compute misses), then fold.
	paths := make([]string, 0, len(matches))
	for _, e := range matches {
		paths = append(paths, filepath.Join(absDir, e.Name()))
	}

	var fileHashes map[string]string
	if cache != nil {
		var err error
		fileHashes, err = cache.BatchHashedFiles(ctx, paths)
		if err != nil {
			return "", err
		}
	} else {
		fileHashes = make(map[string]string, len(paths))
		for _, p := range paths {
			fh, err := hashFileBLAKE3(p)
			if err != nil {
				return "", err
			}
			fileHashes[p] = fh
		}
	}

	h := blake3.New(32, nil)
	for _, e := range matches {
		name := e.Name()
		abs := filepath.Join(absDir, name)
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		if fh, ok := fileHashes[abs]; ok {
			if raw, err := hex.DecodeString(fh); err == nil {
				_, _ = h.Write(raw)
			}
		}
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolveMigrationDirs glob-expands spec.MigrationDirs against
// repoRoot and returns the sorted, deduped set of absolute dir paths.
func resolveMigrationDirs(repoRoot string, spec Spec) ([]string, error) {
	seen := map[string]struct{}{}
	for _, dirGlob := range spec.MigrationDirs {
		matches, err := doublestar.FilepathGlob(filepath.Join(repoRoot, dirGlob))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			abs, err := filepath.Abs(m)
			if err != nil {
				continue
			}
			seen[abs] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// filterMigrationEntries keeps only regular files whose names match
// spec.FileGlobs.
func filterMigrationEntries(entries []os.DirEntry, spec Spec) []os.DirEntry {
	matcher := compiledMatcherFor(spec)
	out := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if matcher(e.Name()) {
			out = append(out, e)
		}
	}
	return out
}

// EnumerateMigrations returns the sorted rel-paths of every file
// under spec.MigrationDirs that matches spec.FileGlobs. Used by
// the watcher's delta replay path.
func EnumerateMigrations(repoRoot string, spec Spec) ([]string, error) {
	dirs, err := resolveMigrationDirs(repoRoot, spec)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, abs := range dirs {
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			continue
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			continue
		}
		for _, e := range filterMigrationEntries(entries, spec) {
			rel, err := filepath.Rel(repoRoot, filepath.Join(abs, e.Name()))
			if err != nil {
				continue
			}
			seen[filepath.ToSlash(rel)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// Per-Spec compiled matcher cache. Keyed by (spec name + glob list)
// so YAML overrides don't collide with built-in spec entries.
var (
	matcherMu    sync.Mutex
	matcherCache = map[string]func(string) bool{}
)

func compiledMatcherFor(spec Spec) func(string) bool {
	key := spec.Name + "\x00" + strings.Join(spec.FileGlobs, "\x00")
	matcherMu.Lock()
	defer matcherMu.Unlock()
	if m, ok := matcherCache[key]; ok {
		return m
	}
	patterns := make([]string, 0, len(spec.FileGlobs)*2)
	for _, g := range spec.FileGlobs {
		patterns = append(patterns, g)
		for alt := range strings.SplitSeq(g, "|") {
			alt = strings.TrimSpace(alt)
			if alt != "" && alt != g {
				patterns = append(patterns, alt)
			}
		}
	}
	matcher := func(name string) bool {
		if len(patterns) == 0 {
			return true
		}
		for _, p := range patterns {
			if ok, _ := doublestar.Match(p, name); ok {
				return true
			}
		}
		return false
	}
	matcherCache[key] = matcher
	return matcher
}

// hashFileBLAKE3 streams `path` through a 256-bit BLAKE3 hasher.
// Used when no HashCache is wired (test paths, non-daemon callers).
func hashFileBLAKE3(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := blake3.New(32, nil)
	// 1 MiB buffer: BLAKE3 hashes far faster than the default 32 KiB
	// io.Copy chunk can feed it, so a bigger buffer cuts read syscalls
	// on large migration files. Mirrors store.hashFileBLAKE3.
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
