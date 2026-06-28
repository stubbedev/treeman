package wt

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
func BringInFiles(ctx context.Context, repoRoot, wtPath string, paths []string, mode string, sink Sink) error {
	_, err := BringInFilesReport(ctx, repoRoot, wtPath, paths, mode, sink)
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
//
// ctx cancellation is honored between entries and (for copy mode) between
// files — a concurrent teardown that cancels an in-flight create finalize
// stops the recursive copy promptly instead of running it to completion
// and resurrecting a dir the teardown just removed.
//
// Entries run concurrently (bounded by bringInEntryFanout): a small
// `copies:` entry no longer waits behind a multi-second node_modules/
// copy. Each copy entry still fans its own files across copyFanout
// workers, so total file-copy concurrency is bounded by the product;
// both caps are modest because bring-in is small-file syscall-bound, not
// bandwidth-bound. results[i] keeps input order. On error the first
// error is returned with the partial report; a failing entry cancels the
// shared ctx so queued entries stop early (their slot stays the
// pre-seeded zero-result, which sums to nothing for the caller).
func BringInFilesReport(ctx context.Context, repoRoot, wtPath string, paths []string, mode string, sink Sink) ([]BringInResult, error) {
	if sink == nil {
		sink = NoopSink{}
	}
	results := make([]BringInResult, len(paths))
	for i, rel := range paths {
		results[i] = BringInResult{Rel: rel, Mode: mode}
	}
	var (
		mu          sync.Mutex
		broughtRels []string
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(bringInEntryFanout())
	for i, rel := range paths {
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			res, rels, err := bringInOneEntry(gctx, repoRoot, wtPath, rel, mode, sink)
			results[i] = res
			if len(rels) > 0 {
				mu.Lock()
				broughtRels = append(broughtRels, rels...)
				mu.Unlock()
			}
			return err
		})
	}
	brErr := g.Wait()

	// Best-effort: hide the brought-in paths from git so the worktree never
	// reads as dirty. A failure here must not fail the bring-in. Run even on
	// brErr so partially-brought entries still get excluded.
	if err := ensureRepoExcludes(repoRoot, broughtRels); err != nil {
		sink.Warn("could not update git exclude for brought-in files: %v", err)
	}

	return results, brErr
}

// bringInEntryFanout bounds how many links/copies entries are brought in
// concurrently. Kept small (it multiplies with copyFanout for copy mode)
// and floored at 2 so single-entry configs see no overhead.
func bringInEntryFanout() int {
	n := runtime.NumCPU() / 4
	switch {
	case n < 2:
		return 2
	case n > 4:
		return 4
	default:
		return n
	}
}

// bringInOneEntry brings a single configured `rel` entry into wtPath,
// expanding glob meta against repoRoot. Returns its BringInResult, the
// repo-relative paths it touched (for git exclude — collected whether
// newly brought or already present, both read as untracked), and the
// first error. On error the partial result up to the failing source is
// returned alongside it.
func bringInOneEntry(ctx context.Context, repoRoot, wtPath, rel, mode string, sink Sink) (BringInResult, []string, error) {
	res := BringInResult{Rel: rel, Mode: mode}
	start := time.Now()
	var broughtRels []string
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
			return res, broughtRels, fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		switch mode {
		case "link":
			if err := os.Symlink(src, dst); err != nil {
				// Concurrent entries (now parallel) can race to the same
				// dst when configs overlap (e.g. `vendor` + `vendor/foo`)
				// — the loser sees EEXIST. Treat as the skip the prior
				// stat check would have produced, not a hard error.
				if os.IsExist(err) {
					res.Skipped++
					continue
				}
				res.DurationMs = time.Since(start).Milliseconds()
				return res, broughtRels, fmt.Errorf("symlink %s → %s: %w", dst, src, err)
			}
			res.Brought++
			res.Files++
		case "copy":
			files, bytes, err := copyPath(ctx, src, dst, info)
			res.Files += files
			res.Bytes += bytes
			if err != nil {
				res.DurationMs = time.Since(start).Milliseconds()
				return res, broughtRels, fmt.Errorf("copy %s → %s: %w", src, dst, err)
			}
			res.Brought++
		}
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, broughtRels, nil
}

// copyFanout bounds how many regular-file copies run concurrently
// inside one copyPath call. Bring-in trees are typically node_modules
// / vendor style — tens of thousands of small files where per-file
// syscall latency, not bandwidth, dominates — so the fan-out recovers
// most of the wall-clock without flooding the page cache or fd table.
// Scaled with core count (a proxy for the host's I/O parallelism) since
// syscall-bound copies benefit from more in-flight work than a fixed 8
// on bigger machines; floored at 8 (the prior constant) and capped at
// 16 so the fd table and page cache stay bounded, especially given this
// multiplies with bringInEntryFanout across concurrent entries.
func copyFanout() int {
	n := runtime.NumCPU()
	switch {
	case n < 8:
		return 8
	case n > 16:
		return 16
	default:
		return n
	}
}

// copyPath copies src → dst, returning the count of regular files
// written and total bytes copied (for observability). Regular files
// are copied with the source's mode preserved — reflinked when the
// filesystem supports it, byte-for-byte otherwise — and fan out across
// copyFanout workers; directories are recursed; symlinks in the source
// tree are recreated as symlinks pointing at the same target (counted
// as a file, 0 bytes). Directory structure and symlinks are created
// synchronously during the walk so every queued file copy already has
// its parent directory in place.
func copyPath(ctx context.Context, src, dst string, info os.FileInfo) (files int, bytes int64, err error) {
	var (
		nFiles atomic.Int64
		nBytes atomic.Int64
	)
	// errgroup.WithContext so a cancelled parent ctx (teardown preempting
	// an in-flight finalize) aborts queued + pending file copies instead of
	// draining the whole fan-out.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(copyFanout())
	walkErr := copyWalk(gctx, src, dst, info, g, &nFiles, &nBytes)
	copyErr := g.Wait()
	if walkErr == nil {
		walkErr = copyErr
	}
	return int(nFiles.Load()), nBytes.Load(), walkErr
}

// copyWalk recursively materializes dirs + symlinks inline and queues
// regular-file copies on g. Counters track successful work only.
// ctx is checked at each node so a cancellation aborts the walk mid-tree.
func copyWalk(ctx context.Context, src, dst string, info os.FileInfo, g *errgroup.Group, files, bytes *atomic.Int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
			if err := copyWalk(ctx, childSrc, childDst, childInfo, g, files, bytes); err != nil {
				return err
			}
		}
		return nil
	case mode.IsRegular():
		perm := mode.Perm()
		g.Go(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
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
