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
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"

	"github.com/stubbedev/treeman/internal/config"
)

// Event is one debounced filesystem event ready for dispatch.
// The cache-hit / cold-build decision is derived from the input
// fingerprint downstream — there's no per-event mode override.
type Event struct {
	RepoPath string
	Path     string
	// DBIndex is the index into cfg.Databases for the owning DB.
	DBIndex int
	// Label is the optional label on the matched Input, referenced
	// by `hooks.on-file-change[].match:` for filtered dispatch.
	Label string
}

// Dispatcher is the callback invoked once per debounced event. The
// daemon implements this with the prepare orchestrator.
type Dispatcher func(ctx context.Context, ev Event) error

// Watcher tails one repo. Call Start in a goroutine; Stop to cancel.
type Watcher struct {
	repoPath string
	paths    []config.WatcherPath
	dispatch Dispatcher
	debounce time.Duration

	fsw *fsnotify.Watcher

	// Pending events keyed by (path, mode) so duplicates inside the
	// debounce window collapse.
	mu      sync.Mutex
	pending map[string]Event
}

// New constructs a Watcher. `paths` is the aggregated list of
// WatcherPath entries (each one carries its DBIndex + optional
// Label) the daemon collected by walking every database's
// `inputs:` block. `debounceMs` is the coalesce window in ms (0 →
// 500 ms default).
func New(repoPath string, paths []config.WatcherPath, debounceMs uint64, dispatch Dispatcher) (*Watcher, error) {
	if dispatch == nil {
		return nil, errors.New("watcher: nil dispatcher")
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify new: %w", err)
	}
	// Clamp before the int64 conversion so a pathological config value
	// can't wrap time.Duration. maxDebounceMs is the largest ms window
	// that still multiplies cleanly into an int64 nanosecond duration.
	const maxDebounceMs = uint64(math.MaxInt64 / int64(time.Millisecond))
	if debounceMs > maxDebounceMs {
		debounceMs = maxDebounceMs
	}
	debounce := time.Duration(debounceMs) * time.Millisecond
	if debounce == 0 {
		debounce = 500 * time.Millisecond
	}
	return &Watcher{
		repoPath: repoPath,
		paths:    paths,
		dispatch: dispatch,
		debounce: debounce,
		fsw:      fsw,
		pending:  map[string]Event{},
	}, nil
}

// Start runs the watch loop until ctx is cancelled or fsnotify
// errors fatally. Adds every existing dir under each glob's prefix
// to fsnotify on first call (fsnotify is non-recursive on Linux);
// new subdirectories are picked up via the Create event handler.
func (w *Watcher) Start(ctx context.Context) error {
	defer func() { _ = w.fsw.Close() }()

	if len(w.paths) == 0 {
		// No globs declared — nothing to watch. Sleep until ctx
		// cancels so the WatcherEntry tracking stays alive (caller
		// uses our presence to count active watchers).
		<-ctx.Done()
		return nil
	}

	w.addAllDirs()

	// Debounce timer, armed only on the first event of a burst and
	// disarmed after each flush. This coalesces a burst (e.g. a `git
	// pull` dropping 40 migration files) into one flush ~debounceMs
	// after the first event — the same bounded latency the previous
	// always-on ticker gave, minus a wakeup every debounceMs while the
	// repo sits idle (which, across many watched worktrees, was pure
	// scheduler churn). Start the timer disarmed.
	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	armed := false

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
			dbIdx, label, matched := w.classify(ev.Name)
			if !matched {
				continue
			}
			w.mu.Lock()
			w.pending[ev.Name] = Event{
				RepoPath: w.repoPath, Path: ev.Name, DBIndex: dbIdx, Label: label,
			}
			w.mu.Unlock()
			// Arm the debounce window on the first event of a burst.
			// Subsequent events ride the same window (no reset) so a
			// steady stream can't starve the flush past debounceMs.
			// Resetting only when disarmed keeps the timer channel
			// drained, so Reset never races a pending tick.
			if !armed {
				timer.Reset(w.debounce)
				armed = true
			}

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("fsnotify error", "repo", w.repoPath, "err", err)

		case <-timer.C:
			armed = false
			w.flush(ctx)
		}
	}
}

// Stop closes the underlying fsnotify.Watcher; Start unblocks.
func (w *Watcher) Stop() { _ = w.fsw.Close() }

// noiseDirNames is the set of directory basenames we refuse to
// recurse into during addAllDirs. fsnotify is non-recursive on Linux
// but the WalkDir below would still descend node_modules/ and
// vendor/ if a glob's static prefix happened to enclose them — and
// even one inotify watch on a deep node_modules generates wakeups
// every time webpack or composer touches it. Filtering at subscribe
// time means the kernel never delivers the events at all.
var noiseDirNames = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	"vendor":       {},
	".idea":        {},
	".vscode":      {},
	"dist":         {},
	"build":        {},
	"target":       {},
	".next":        {},
	".turbo":       {},
	".cache":       {},
	".direnv":      {},
}

// addAllDirs walks each glob's longest static prefix and adds every
// existing directory to fsnotify. Globs are matched lazily on
// emit. We add the parent dirs only — fsnotify reports events on
// files within an added dir without recursing.
func (w *Watcher) addAllDirs() {
	added := map[string]struct{}{}
	for _, p := range w.paths {
		// Take the longest path prefix without a glob meta-character
		// so we anchor on something concrete (e.g.
		// `database/migrations/**` → `database/migrations`).
		base := filepath.Join(w.repoPath, staticPrefix(p.Glob))
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // skip unreadable entries and keep walking
			}
			if !d.IsDir() {
				return nil
			}
			// Skip noise dirs entirely — never subscribe inside them.
			// Check by basename so the filter works regardless of how
			// deep the static prefix nested. The base of the walk
			// itself is always added (a user who points a glob
			// directly at `node_modules/**` knows what they want).
			if path != base {
				if _, noisy := noiseDirNames[d.Name()]; noisy {
					return filepath.SkipDir
				}
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
}

// classify resolves an event path against the configured globs and
// returns the first matching DB index + label. Unmatched paths
// drop on the floor (the user explicitly listed which paths matter
// via inputs:; everything else is noise).
func (w *Watcher) classify(path string) (int, string, bool) {
	rel, err := filepath.Rel(w.repoPath, path)
	if err != nil {
		return 0, "", false
	}
	rel = filepath.ToSlash(rel)
	for _, p := range w.paths {
		match, _ := doublestar.PathMatch(p.Glob, rel)
		if !match {
			continue
		}
		return p.DBIndex, p.Label, true
	}
	return 0, "", false
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
			slog.Warn("watcher dispatch", "path", e.Path, "label", e.Label, "err", err)
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
