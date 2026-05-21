package framework

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// MigrationsHash fingerprints the set of migration files claimed by
// `spec` under `repoRoot`. The output is the hex SHA-256 of:
//
//	"<rel-path>\0" + (HashMode=filename ? "" : "<sha256-of-bytes>") + "\n"
//
// concatenated over every matching file, sorted by rel-path. Used as
// part of the snapshot cache key — two prepare runs sharing the same
// migrations hash + dump hash can skip the cold build and just clone
// the cached template.
//
// HashFilename mode skips file IO entirely (Laravel/Rails/Django add
// new files but don't mutate old ones, so filename alone is a sound
// invariant). HashChecksum hashes the bytes (sqlx-cli/Flyway mutate
// in place).
func MigrationsHash(repoRoot string, spec Spec) (string, error) {
	files, err := EnumerateMigrations(repoRoot, spec)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, rel := range files {
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		if spec.HashMode == HashChecksum {
			abs := filepath.Join(repoRoot, rel)
			f, err := os.Open(abs)
			if err != nil {
				return "", err
			}
			fh := sha256.New()
			if _, err := io.Copy(fh, f); err != nil {
				_ = f.Close()
				return "", err
			}
			_ = f.Close()
			_, _ = h.Write(fh.Sum(nil))
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
