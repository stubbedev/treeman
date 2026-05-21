// Package watcher tails filesystem events under a repo's main
// checkout and dispatches `prepare` reruns for every active worktree
// of that repo whenever a migration directory, seed dump, or
// lockfile changes.
//
// One `Watcher` per repo. Globs from `watcher.paths` decide which
// events the watcher cares about; each match resolves to a
// dispatch mode (`auto` | `delta` | `rebuild`). Events are debounced
// per (worktree, mode) pair so a `git pull` that drops 40 migration
// files at once collapses into one prepare run per worktree.
//
// The watcher coexists with the MySQL binlog tailer in
// `internal/db/binlog`: the binlog handles DDL applied directly to
// the source DB (e.g. someone running `php artisan migrate` in a
// worktree), while this watcher handles edits to the migration
// SOURCE files (someone adding a new migration to git). Both
// pathways are needed because each catches the other's blind spot.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"

	"github.com/stubbedev/treeman/internal/config"
)

// DispatchMode classifies what the watcher wants done. The actual
// `prepare`-vs-`delta` decision lives in the Dispatcher callback;
// this is the watcher's hint based on the glob's `on:` field.
type DispatchMode string

const (
	ModeAuto    DispatchMode = "auto"
	ModeDelta   DispatchMode = "delta"
	ModeRebuild DispatchMode = "rebuild"
)

// Event is one debounced filesystem event ready for dispatch.
type Event struct {
	RepoPath string
	Path     string
	Mode     DispatchMode
}

// Dispatcher is the callback invoked once per debounced event. The
// daemon implements this with the prepare orchestrator.
type Dispatcher func(ctx context.Context, ev Event) error

// Watcher tails one repo. Call Start in a goroutine; Stop to cancel.
type Watcher struct {
	repoPath   string
	cfg        config.WatcherConfig
	dispatch   Dispatcher
	debounceMs time.Duration

	fsw *fsnotify.Watcher

	// Pending events keyed by (path, mode) so duplicates inside the
	// debounce window collapse.
	mu      sync.Mutex
	pending map[string]Event
}

// New constructs a Watcher. Call Start to begin streaming.
func New(repoPath string, cfg config.WatcherConfig, dispatch Dispatcher) (*Watcher, error) {
	if dispatch == nil {
		return nil, errors.New("watcher: nil dispatcher")
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify new: %w", err)
	}
	debounce := time.Duration(cfg.DebounceMs) * time.Millisecond
	if debounce == 0 {
		debounce = 500 * time.Millisecond
	}
	return &Watcher{
		repoPath:   repoPath,
		cfg:        cfg,
		dispatch:   dispatch,
		debounceMs: debounce,
		fsw:        fsw,
		pending:    map[string]Event{},
	}, nil
}

// Start runs the watch loop until ctx is cancelled or fsnotify
// errors fatally. Adds every existing dir under each glob's prefix
// to fsnotify on first call (fsnotify is non-recursive on Linux);
// new subdirectories are picked up via the Create event handler.
func (w *Watcher) Start(ctx context.Context) error {
	defer w.fsw.Close()

	if len(w.cfg.Paths) == 0 {
		// No globs declared — nothing to watch. Sleep until ctx
		// cancels so the WatcherEntry tracking stays alive (caller
		// uses our presence to count active watchers).
		<-ctx.Done()
		return nil
	}

	if err := w.addAllDirs(); err != nil {
		return fmt.Errorf("watcher add dirs: %w", err)
	}

	ticker := time.NewTicker(w.debounceMs)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			// If a new directory was created inside a tracked
			// subtree, add it so we get events from inside it on
			// Linux (Darwin's kqueue auto-recurses).
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					_ = w.fsw.Add(ev.Name)
				}
			}
			mode, matched := w.classify(ev.Name)
			if !matched {
				continue
			}
			w.mu.Lock()
			w.pending[ev.Name+"\x00"+string(mode)] = Event{
				RepoPath: w.repoPath, Path: ev.Name, Mode: mode,
			}
			w.mu.Unlock()

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("fsnotify error", "repo", w.repoPath, "err", err)

		case <-ticker.C:
			w.flush(ctx)
		}
	}
}

// Stop closes the underlying fsnotify.Watcher; Start unblocks.
func (w *Watcher) Stop() { _ = w.fsw.Close() }

// addAllDirs walks each glob's longest static prefix and adds every
// existing directory to fsnotify. Globs are matched lazily on
// emit. We add the parent dirs only — fsnotify reports events on
// files within an added dir without recursing.
func (w *Watcher) addAllDirs() error {
	added := map[string]struct{}{}
	for _, p := range w.cfg.Paths {
		// Take the longest path prefix without a glob meta-character
		// so we anchor on something concrete (e.g.
		// `database/migrations/**` → `database/migrations`).
		base := filepath.Join(w.repoPath, staticPrefix(p.Glob))
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			if _, seen := added[path]; seen {
				return nil
			}
			added[path] = struct{}{}
			if err := w.fsw.Add(path); err != nil {
				slog.Debug("fsnotify add skipped", "path", path, "err", err)
			}
			return nil
		})
	}
	return nil
}

// classify resolves an event path against the configured globs and
// returns the first matching mode. Unmatched paths drop on the
// floor (`auto` for everything would be too noisy — `.git`,
// `node_modules` etc.).
func (w *Watcher) classify(path string) (DispatchMode, bool) {
	rel, err := filepath.Rel(w.repoPath, path)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	for _, p := range w.cfg.Paths {
		match, _ := doublestar.PathMatch(p.Glob, rel)
		if !match {
			continue
		}
		mode := ModeAuto
		switch p.On {
		case "delta":
			mode = ModeDelta
		case "rebuild":
			mode = ModeRebuild
		case "", "auto":
			mode = ModeAuto
		}
		return mode, true
	}
	return "", false
}

// flush dispatches every pending event then clears the buffer.
// Runs sequentially: dispatch is expected to be fast (it enqueues
// work to background goroutines, not blocking).
func (w *Watcher) flush(ctx context.Context) {
	w.mu.Lock()
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return
	}
	events := make([]Event, 0, len(w.pending))
	for _, e := range w.pending {
		events = append(events, e)
	}
	w.pending = map[string]Event{}
	w.mu.Unlock()

	for _, e := range events {
		if err := w.dispatch(ctx, e); err != nil {
			slog.Warn("watcher dispatch", "path", e.Path, "mode", e.Mode, "err", err)
		}
	}
}

// staticPrefix returns the longest leading directory prefix of
// `glob` that contains no glob meta-characters.
func staticPrefix(glob string) string {
	for i, c := range glob {
		switch c {
		case '*', '?', '[', '{':
			// Trim back to the last separator before this char.
			cut := glob[:i]
			if idx := lastSep(cut); idx >= 0 {
				return cut[:idx]
			}
			return ""
		}
	}
	return glob
}

func lastSep(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' || s[i] == filepath.Separator {
			return i
		}
	}
	return -1
}
