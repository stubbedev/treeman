package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// HeadWatcher tails `.git/HEAD` (resolved through the gitlink for
// linked worktrees) and invokes `onChange` whenever the file's
// contents change. Used by the daemon to re-run finalize (patches +
// prepare) when a user switches branches inside an existing
// worktree.
//
// Implementation note: git rewrites HEAD atomically (tmp file +
// rename), which invalidates an fsnotify watch on the file itself.
// We watch the *parent directory* and filter events to the HEAD
// path; on every matching event we re-read HEAD and compare to the
// last seen value before dispatching.
type HeadWatcher struct {
	worktreePath string
	headPath     string // absolute path to the actual HEAD file
	headDir      string // parent dir of headPath (the dir we subscribe to)
	headName     string // basename of headPath
	onChange     func(ctx context.Context, newRef string)

	debounce time.Duration
	mu       sync.Mutex
	lastSeen string
	lastOp   string // last observed in-progress git op (merge/rebase/…), "" when clean
	pending  *time.Timer
	fsw      *fsnotify.Watcher
}

// resolveHeadPath returns the absolute path of the HEAD file for
// `worktreePath`. Handles the gitlink case (`.git` is a regular
// file pointing at `<common-dir>/worktrees/<name>/`) by following
// the `gitdir:` redirect once.
func resolveHeadPath(worktreePath string) (string, error) {
	dotGit := filepath.Join(worktreePath, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", fmt.Errorf("stat .git: %w", err)
	}
	if info.IsDir() {
		return filepath.Join(dotGit, "HEAD"), nil
	}
	// Gitlink: file containing `gitdir: <path>`.
	body, err := os.ReadFile(dotGit)
	if err != nil {
		return "", fmt.Errorf("read gitlink: %w", err)
	}
	line := strings.TrimSpace(string(body))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("gitlink missing %q prefix: %q", prefix, line)
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktreePath, gitdir)
	}
	return filepath.Join(gitdir, "HEAD"), nil
}

// NewHeadWatcher constructs a HeadWatcher. Returns an error when the
// HEAD path can't be resolved (corrupt .git pointer) or when
// fsnotify init fails.
func NewHeadWatcher(worktreePath string, debounce time.Duration, onChange func(ctx context.Context, newRef string)) (*HeadWatcher, error) {
	headPath, err := resolveHeadPath(worktreePath)
	if err != nil {
		return nil, err
	}
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify new: %w", err)
	}
	return &HeadWatcher{
		worktreePath: worktreePath,
		headPath:     headPath,
		headDir:      filepath.Dir(headPath),
		headName:     filepath.Base(headPath),
		onChange:     onChange,
		debounce:     debounce,
		fsw:          fsw,
	}, nil
}

// Start subscribes to the HEAD's parent directory and loops until
// ctx is cancelled. The first read seeds `lastSeen`, so the initial
// HEAD state never fires `onChange` — only subsequent edits do.
func (h *HeadWatcher) Start(ctx context.Context) error {
	defer func() { _ = h.fsw.Close() }()

	if err := h.fsw.Add(h.headDir); err != nil {
		return fmt.Errorf("watcher add %s: %w", h.headDir, err)
	}

	// Seed lastSeen with the initial HEAD so the first real edit is
	// the first dispatched change. Seed lastOp too (h.headDir IS the
	// per-worktree git dir, where the op markers live) so a merge
	// already in progress at boot is recognised as the "busy" baseline
	// — its later clear then dispatches the recovery re-run.
	if v, err := os.ReadFile(h.headPath); err == nil {
		h.mu.Lock()
		h.lastSeen = strings.TrimSpace(string(v))
		h.mu.Unlock()
	}
	h.mu.Lock()
	h.lastOp = gitOpInProgress(h.headDir)
	h.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-h.fsw.Events:
			if !ok {
				return nil
			}
			// HEAD edits AND git-op markers (MERGE_HEAD, rebase-*/,
			// CHERRY_PICK_HEAD, REVERT_HEAD) are interesting — the
			// markers live in this same dir and signal the start/clear
			// of a merge/rebase. fsnotify reports the post-rename name
			// on macOS / Linux when git does its tmp + rename dance, so
			// basename matching is reliable in practice.
			base := filepath.Base(ev.Name)
			if base != h.headName && !isGitOpMarker(base) {
				continue
			}
			// Remove matters here: a `git merge --abort` (or the commit
			// that finishes a merge) deletes MERGE_HEAD, and that clear
			// is the trigger for the recovery re-run.
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			h.schedule(ctx)
		case err, ok := <-h.fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("head watcher fsnotify error", "wt", h.worktreePath, "err", err)
		}
	}
}

// Stop closes the underlying fsnotify watcher; Start unblocks.
func (h *HeadWatcher) Stop() { _ = h.fsw.Close() }

// schedule resets the debounce timer. On expiry, fire() reads HEAD
// and dispatches `onChange` if the value moved.
func (h *HeadWatcher) schedule(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending != nil {
		h.pending.Stop()
	}
	h.pending = time.AfterFunc(h.debounce, func() { h.fire(ctx) })
}

func (h *HeadWatcher) fire(ctx context.Context) {
	cur := ""
	if v, err := os.ReadFile(h.headPath); err == nil {
		cur = strings.TrimSpace(string(v))
	}
	curOp := gitOpInProgress(h.headDir)

	h.mu.Lock()
	prev, prevOp := h.lastSeen, h.lastOp
	headChanged := cur != "" && cur != prev
	if cur != "" {
		h.lastSeen = cur
	}
	h.lastOp = curOp
	h.mu.Unlock()

	switch {
	case curOp != "":
		// A merge/rebase is now in progress. Suppress the dispatch —
		// finalize would defer against the conflicted tree anyway, and
		// the recovery comes when the op clears. Tracking lastOp here is
		// what makes that later clear observable.
		slog.Info("head watcher: git op in progress, deferring re-run",
			"wt", h.worktreePath, "op", curOp)
	case prevOp != "":
		// Op just cleared (curOp == "" here). `git merge --abort`
		// restores the original commit so HEAD never moves — without
		// this branch a deferred/failed worktree would stay stuck. A
		// resolve+commit moves HEAD too, but firing once on the clear
		// is correct either way (finalize is idempotent).
		slog.Info("head watcher: git op cleared, re-running finalize",
			"wt", h.worktreePath, "from_op", prevOp, "head", cur)
		h.onChange(ctx, cur)
	case headChanged:
		slog.Info("head changed", "wt", h.worktreePath, "from", prev, "to", cur)
		h.onChange(ctx, cur)
	}
}
