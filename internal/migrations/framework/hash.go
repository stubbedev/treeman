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
// needs: a stat-gated, content-addressed file-hash cache. Defined here
// so the framework package doesn't depend on internal/store.
type HashCache interface {
	HashedFile(ctx context.Context, path string) (string, error)
	BatchHashedFiles(ctx context.Context, paths []string) (map[string]string, error)
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
//	"<basename>\0" + "<blake3-of-bytes>" + "\n"
//
// The fingerprint is purely content-derived: every member file is
// content-hashed (the per-file work is cached in `file_hashes`,
// stat-gated on each file's own size+mtime). There is deliberately no
// directory-level (mtime, member-count) short-circuit — an in-place
// content edit bumps the *file* mtime but not the parent directory's
// mtime on Linux or macOS, so a dir-level gate would silently reuse a
// stale digest and skip the rebuild. Content-only is the invariant.
func MigrationsHashWithCache(ctx context.Context, cache HashCache, repoRoot string, spec Spec) (string, error) {
	dirs, err := resolveMigrationDirs(repoRoot, spec)
	if err != nil {
		return "", err
	}

	type dirEntry struct {
		absDir string
		relDir string
	}
	dirRecords := make([]dirEntry, 0, len(dirs))
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
		})
	}
	sort.SliceStable(dirRecords, func(i, j int) bool { return dirRecords[i].relDir < dirRecords[j].relDir })

	perDirDigest := make(map[string]string, len(dirRecords))
	for i := range dirRecords {
		d := &dirRecords[i]
		entries, err := os.ReadDir(d.absDir)
		if err != nil {
			continue
		}
		matches := filterMigrationEntries(entries, spec)
		digest, err := computeDirDigest(ctx, cache, d.absDir, matches)
		if err != nil {
			return "", err
		}
		perDirDigest[d.relDir] = digest
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

// computeDirDigest produces the per-directory digest by content-hashing
// every migration file in sorted order. The historical `filename` mode
// (hash filenames only) is gone — it relied on an append-only
// convention that wasn't enforced, so an in-place edit could silently
// keep an old cached template alive.
func computeDirDigest(ctx context.Context, cache HashCache, absDir string, matches []os.DirEntry) (string, error) {
	if len(matches) == 0 {
		// Stable empty-dir digest so an empty migrations folder
		// contributes a constant per-dir hash.
		h := blake3.New(32, nil)
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Name() < matches[j].Name() })

	// Content hash: collect absolute paths, batch-lookup hashes
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
