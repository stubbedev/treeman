package framework

import (
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"golang.org/x/sync/errgroup"
	"lukechampine.com/blake3"
)

// HashCache is the subset of *store.Store that MigrationsHashWithCache
// needs. Defined here so the framework package doesn't depend on
// internal/store.
type HashCache interface {
	HashedFile(ctx context.Context, path string) (string, error)
}

// MigrationsHash fingerprints the set of migration files claimed by
// `spec` under `repoRoot`. Wraps MigrationsHashWithCache with no
// cache — kept for callers that don't have a Store handle.
func MigrationsHash(repoRoot string, spec Spec) (string, error) {
	return MigrationsHashWithCache(context.Background(), nil, repoRoot, spec)
}

// MigrationsHashWithCache fingerprints the migration set keyed by
// `spec`. The output is the hex BLAKE3-256 of:
//
//	"<rel-path>\0" + (HashMode=filename ? "" : "<blake3-of-bytes>") + "\n"
//
// concatenated over every matching file, sorted by rel-path.
//
// HashFilename mode skips file IO entirely (Laravel/Rails/Django add
// new files but don't mutate old ones, so filename alone is a sound
// invariant). HashChecksum hashes the bytes; with a non-nil cache
// the per-file digest is looked up via the stat-gated SQLite cache
// so unchanged migrations skip the disk read, and the file hashes
// run in parallel across NumCPU workers.
func MigrationsHashWithCache(ctx context.Context, cache HashCache, repoRoot string, spec Spec) (string, error) {
	files, err := EnumerateMigrations(repoRoot, spec)
	if err != nil {
		return "", err
	}

	if spec.HashMode != HashChecksum {
		h := blake3.New(32, nil)
		for _, rel := range files {
			_, _ = h.Write([]byte(rel))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte{'\n'})
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	// HashChecksum: compute per-file digests in parallel, then fold
	// them into the outer hash in sorted rel-path order.
	fileHashes := make([]string, len(files))
	limit := runtime.GOMAXPROCS(0)
	if limit < 1 {
		limit = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for i, rel := range files {
		i, rel := i, rel
		g.Go(func() error {
			abs := filepath.Join(repoRoot, rel)
			if cache != nil {
				absAbs, err := filepath.Abs(abs)
				if err == nil {
					if h, err := cache.HashedFile(gctx, absAbs); err == nil {
						fileHashes[i] = h
						return nil
					}
				}
			}
			h, err := hashFileBLAKE3(abs)
			if err != nil {
				return err
			}
			fileHashes[i] = h
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return "", err
	}

	h := blake3.New(32, nil)
	for i, rel := range files {
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		// fileHashes[i] is the per-file BLAKE3-hex; decode back to
		// bytes so the outer hash mixes the raw 32-byte digest the
		// way the previous sha256 implementation did.
		if raw, err := hex.DecodeString(fileHashes[i]); err == nil {
			_, _ = h.Write(raw)
		}
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EnumerateMigrations returns the sorted rel-paths of every file
// under spec.MigrationDirs that matches spec.FileGlobs. Used by
// MigrationsHash and the watcher's delta replay path.
func EnumerateMigrations(repoRoot string, spec Spec) ([]string, error) {
	seen := map[string]struct{}{}
	for _, dirGlob := range spec.MigrationDirs {
		matches, err := doublestar.FilepathGlob(filepath.Join(repoRoot, dirGlob))
		if err != nil {
			return nil, err
		}
		for _, dir := range matches {
			fi, err := os.Stat(dir)
			if err != nil || !fi.IsDir() {
				continue
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if !matchAnyGlob(name, spec.FileGlobs) {
					continue
				}
				abs := filepath.Join(dir, name)
				rel, err := filepath.Rel(repoRoot, abs)
				if err != nil {
					continue
				}
				seen[filepath.ToSlash(rel)] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

func matchAnyGlob(name string, globs []string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		// Direct match.
		if ok, _ := doublestar.Match(g, name); ok {
			return true
		}
		// Tolerate `*.ext|*.ext2` style though Spec doesn't use it.
		for _, alt := range strings.Split(g, "|") {
			if ok, _ := doublestar.Match(strings.TrimSpace(alt), name); ok {
				return true
			}
		}
	}
	return false
}

// hashFileBLAKE3 streams `path` through a 256-bit BLAKE3 hasher.
// Used when no HashCache is wired (test paths, non-daemon callers).
func hashFileBLAKE3(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := blake3.New(32, nil)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
