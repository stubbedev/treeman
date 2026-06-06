package wt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// hasGlobMeta reports whether `p` carries any glob meta-character we
// support — including `{` (brace alternation) and the second `*` of a
// `**` recursive segment. Kept in sync with the doublestar matcher
// used to expand it, so a pattern is never treated as a literal path
// when doublestar would have expanded it.
func hasGlobMeta(p string) bool {
	return strings.ContainsAny(p, "*?[{")
}

// BringInResult is the per-entry outcome of a BringInFiles pass: one
// per configured `paths` entry, aggregated across any glob matches.
// Callers (the daemon finalize) use it to emit per-stage observability
// events (which entry, how big, how long) without coupling this
// package to the event store.
type BringInResult struct {
	Rel        string // the configured links/copies entry
	Mode       string // "link" | "copy"
	Matches    int    // sources the entry resolved to
	Brought    int    // sources actually linked/copied (dst created)
	Skipped    int    // sources skipped because dst already existed
	Missing    int    // non-glob sources that did not exist
	Files      int    // regular files written (copy mode) / symlinks made
	Bytes      int64  // bytes copied (copy mode)
	DurationMs int64
}

// BringInFiles is the fire-and-forget form of BringInFilesReport: it
// performs the same work and discards the per-entry report. Retained
// for callers (CLI sync path, tests) that don't emit events.
func BringInFiles(repoRoot, wtPath string, paths []string, mode string, sink Sink) error {
	_, err := BringInFilesReport(repoRoot, wtPath, paths, mode, sink)
	return err
}

// BringInFilesReport brings each entry in `paths` from repoRoot into
// wtPath via either symlink (mode="link") or recursive copy
// (mode="copy"), returning a per-entry BringInResult so callers can
// surface timing + volume in the event log. Glob meta-characters
// (`*?[{` plus `**` recursion) expand against repoRoot via doublestar.
// Idempotent — if the destination already exists the entry is skipped.
// Missing non-glob sources are reported via the sink as warnings;
// missing glob expansions are silent. On error the partial report up
// to and including the failing entry is returned alongside the error.
func BringInFilesReport(repoRoot, wtPath string, paths []string, mode string, sink Sink) ([]BringInResult, error) {
	if sink == nil {
		sink = NoopSink{}
	}
	results := make([]BringInResult, 0, len(paths))
	for _, rel := range paths {
		res := BringInResult{Rel: rel, Mode: mode}
		start := time.Now()
		var matches []string
		if hasGlobMeta(rel) {
			// doublestar (not stdlib filepath.Glob) so `**` matches
			// zero-or-more path segments and `{a,b}` alternation works,
			// matching the semantics documented for links/copies globs.
			m, _ := doublestar.FilepathGlob(filepath.Join(repoRoot, rel))
			matches = m
		} else {
			matches = []string{filepath.Join(repoRoot, rel)}
		}
		res.Matches = len(matches)
		for _, src := range matches {
			info, err := os.Stat(src)
			if err != nil {
				if !hasGlobMeta(rel) {
					sink.Warn("%s source missing, skipping: %s", mode, src)
					res.Missing++
				}
				continue
			}
			relToRepo, err := filepath.Rel(repoRoot, src)
			if err != nil {
				relToRepo = filepath.Base(src)
			}
			dst := filepath.Join(wtPath, relToRepo)
			if _, err := os.Stat(dst); err == nil {
				res.Skipped++
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				res.DurationMs = time.Since(start).Milliseconds()
				results = append(results, res)
				return results, fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
			}
			switch mode {
			case "link":
				if err := os.Symlink(src, dst); err != nil {
					res.DurationMs = time.Since(start).Milliseconds()
					results = append(results, res)
					return results, fmt.Errorf("symlink %s → %s: %w", dst, src, err)
				}
				res.Brought++
				res.Files++
			case "copy":
				files, bytes, err := copyPath(src, dst, info)
				res.Files += files
				res.Bytes += bytes
				if err != nil {
					res.DurationMs = time.Since(start).Milliseconds()
					results = append(results, res)
					return results, fmt.Errorf("copy %s → %s: %w", src, dst, err)
				}
				res.Brought++
			}
		}
		res.DurationMs = time.Since(start).Milliseconds()
		results = append(results, res)
	}
	return results, nil
}

// copyPath copies src → dst, returning the count of regular files
// written and total bytes copied (for observability). Regular files
// are copied byte-for-byte with the source's mode preserved;
// directories are recursed; symlinks in the source tree are recreated
// as symlinks pointing at the same target (counted as a file, 0 bytes).
func copyPath(src, dst string, info os.FileInfo) (files int, bytes int64, err error) {
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return 0, 0, err
		}
		if err := os.Symlink(target, dst); err != nil {
			return 0, 0, err
		}
		return 1, 0, nil
	case mode.IsDir():
		if err := os.MkdirAll(dst, mode.Perm()); err != nil {
			return 0, 0, err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return 0, 0, err
		}
		for _, e := range entries {
			childSrc := filepath.Join(src, e.Name())
			childDst := filepath.Join(dst, e.Name())
			childInfo, err := os.Lstat(childSrc)
			if err != nil {
				return files, bytes, err
			}
			cf, cb, err := copyPath(childSrc, childDst, childInfo)
			files += cf
			bytes += cb
			if err != nil {
				return files, bytes, err
			}
		}
		return files, bytes, nil
	case mode.IsRegular():
		n, err := copyRegularFile(src, dst, mode.Perm())
		if err != nil {
			return 0, n, err
		}
		return 1, n, nil
	default:
		return 0, 0, fmt.Errorf("unsupported file type for %s (mode=%v)", src, mode)
	}
}

// copyRegularFile copies src → dst and returns the bytes written.
func copyRegularFile(src, dst string, perm os.FileMode) (int64, error) {
	sf, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() { _ = sf.Close() }()
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(df, sf)
	if err != nil {
		_ = df.Close()
		return n, err
	}
	// Check the close: a deferred close would swallow a failed final
	// flush, leaving dst silently truncated.
	if err := df.Close(); err != nil {
		return n, fmt.Errorf("finalize copy %s: %w", dst, err)
	}
	return n, nil
}

// PruneEmptyParents walks up from `start` removing now-empty
// directories until we leave `wtRoot` (the configured worktrees
// root). Best-effort: any rmdir error stops the walk.
func PruneEmptyParents(start, wtRoot string) {
	if start == "" || wtRoot == "" {
		return
	}
	parent := filepath.Dir(start)
	for {
		rel, err := filepath.Rel(wtRoot, parent)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return
		}
		if err := os.Remove(parent); err != nil {
			return
		}
		parent = filepath.Dir(parent)
	}
}
