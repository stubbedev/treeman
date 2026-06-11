package wt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"golang.org/x/sync/errgroup"
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
	var broughtRels []string
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
			// Keep it out of git status whether we create it now or it
			// already exists from a prior run — both would show as untracked.
			broughtRels = append(broughtRels, relToRepo)
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

	// Best-effort: hide the brought-in paths from git so the worktree never
	// reads as dirty. A failure here must not fail the bring-in.
	if err := ensureRepoExcludes(repoRoot, broughtRels); err != nil {
		sink.Warn("could not update git exclude for brought-in files: %v", err)
	}

	return results, nil
}

// copyFanout bounds how many regular-file copies run concurrently
// inside one copyPath call. Bring-in trees are typically node_modules
// / vendor style — tens of thousands of small files where per-file
// syscall latency, not bandwidth, dominates — so a modest fan-out
// recovers most of the wall-clock without flooding the page cache or
// fd table.
const copyFanout = 8

// copyPath copies src → dst, returning the count of regular files
// written and total bytes copied (for observability). Regular files
// are copied with the source's mode preserved — reflinked when the
// filesystem supports it, byte-for-byte otherwise — and fan out across
// copyFanout workers; directories are recursed; symlinks in the source
// tree are recreated as symlinks pointing at the same target (counted
// as a file, 0 bytes). Directory structure and symlinks are created
// synchronously during the walk so every queued file copy already has
// its parent directory in place.
func copyPath(src, dst string, info os.FileInfo) (files int, bytes int64, err error) {
	var (
		g      errgroup.Group
		nFiles atomic.Int64
		nBytes atomic.Int64
	)
	g.SetLimit(copyFanout)
	walkErr := copyWalk(src, dst, info, &g, &nFiles, &nBytes)
	copyErr := g.Wait()
	if walkErr == nil {
		walkErr = copyErr
	}
	return int(nFiles.Load()), nBytes.Load(), walkErr
}

// copyWalk recursively materializes dirs + symlinks inline and queues
// regular-file copies on g. Counters track successful work only.
func copyWalk(src, dst string, info os.FileInfo, g *errgroup.Group, files, bytes *atomic.Int64) error {
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		files.Add(1)
		return nil
	case mode.IsDir():
		if err := os.MkdirAll(dst, mode.Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childSrc := filepath.Join(src, e.Name())
			childDst := filepath.Join(dst, e.Name())
			childInfo, err := os.Lstat(childSrc)
			if err != nil {
				return err
			}
			if err := copyWalk(childSrc, childDst, childInfo, g, files, bytes); err != nil {
				return err
			}
		}
		return nil
	case mode.IsRegular():
		perm := mode.Perm()
		g.Go(func() error {
			n, err := copyRegularFile(src, dst, perm)
			if err != nil {
				return err
			}
			files.Add(1)
			bytes.Add(n)
			return nil
		})
		return nil
	default:
		return fmt.Errorf("unsupported file type for %s (mode=%v)", src, mode)
	}
}

// copyRegularFile copies src → dst and returns the bytes written.
// It first attempts a reflink clone (constant-time copy-on-write on
// btrfs/XFS — see cloneFile), falling back to a streamed io.Copy on
// filesystems without reflink support.
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
	var n int64
	if cloneErr := cloneFile(df, sf); cloneErr == nil {
		if st, err := sf.Stat(); err == nil {
			n = st.Size()
		}
	} else {
		n, err = io.Copy(df, sf)
		if err != nil {
			_ = df.Close()
			return n, err
		}
	}
	// Check the close: a deferred close would swallow a failed final
	// flush, leaving dst silently truncated.
	if err := df.Close(); err != nil {
		return n, fmt.Errorf("finalize copy %s: %w", dst, err)
	}
	return n, nil
}

// underRoot reports whether path sits strictly under root — not root
// itself, not an escape via "..", and neither side empty.
func underRoot(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != "" && !strings.HasPrefix(rel, "..")
}

// RemoveWorktreeTree deletes the worktree directory at wtPath plus any
// now-empty parent directories up to (but not including) wtRoot.
//
// `git worktree remove` drops the tracked tree and admin files, but
// untracked copies/links bring-in (node_modules, vendored dirs, nested
// git repos) survives even with --force — a non-empty leftover then
// blocks the next create at the same path with "destination path
// already exists". RemoveAll clears it; the parent walk reaps the empty
// feature/ scaffolding dirs git leaves behind.
//
// Guarded to paths strictly under wtRoot, so a bad wtPath (repo root,
// "", or anything outside the worktrees root) is a no-op. Best-effort:
// any rmdir error stops the parent walk.
func RemoveWorktreeTree(wtPath, wtRoot string) {
	if !underRoot(wtPath, wtRoot) {
		return
	}
	_ = os.RemoveAll(wtPath)
	for parent := filepath.Dir(wtPath); underRoot(parent, wtRoot); parent = filepath.Dir(parent) {
		if err := os.Remove(parent); err != nil {
			return
		}
	}
}
