package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
)

// ConfigReloader watches the YAML files that feed `resolve.LoadResolved`
// and reacts to edits by invalidating the resolved-config cache and
// restarting the per-repo watchers that snapshotted their config at
// boot (worktree fsnotify watchers, lifecycle
// watcher).
//
// Two reload entry points:
//
//   - fsnotify events on the watched directories (debounced 500ms).
//   - explicit `ReloadAll` / `ReloadRepo` calls — invoked by SIGHUP and
//     by the `config_reload` RPC.
//
// Directories — not individual files — are added to the fsnotify
// watcher. Most editors (vim's atomic rename, `yamlpatch.AtomicWrite`)
// replace `.treeman.yaml` via rename, which breaks file-level watches
// because the new inode isn't the one being watched. Watching the
// parent and filtering by basename catches every write pattern.
type ConfigReloader struct {
	st       *State
	fsw      *fsnotify.Watcher
	debounce time.Duration

	mu        sync.Mutex
	dirRefs   map[string]int      // dir → refcount (global dir + each enrolled repo)
	repos     map[string]struct{} // repos currently tracked
	globalDir string              // ~/.config/treeman (empty if no $HOME)

	timersMu sync.Mutex
	timers   map[string]*time.Timer // debounce keyed by "" (global) or repoPath

	// reloadLksMu guards reloadLks. Per-repo mutexes serialise
	// concurrent reloadOne goroutines against the same repo so a
	// SIGHUP + RPC + fsnotify burst can't fire three overlapping
	// EnrollMainWorktree / unregister / re-spawn sequences on the
	// same target. Without serialisation the watcher restart can
	// race the worktrees row teardown and leak goroutines.
	reloadLksMu sync.Mutex
	reloadLks   map[string]*sync.Mutex
}

// NewConfigReloader creates the watcher and pre-registers the global
// config directory `~/.config/treeman`. Repo directories are added
// later via `AddRepo`. Returns a non-nil error only when fsnotify
// itself fails to start — a missing global dir is silently skipped.
func NewConfigReloader(st *State) (*ConfigReloader, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	cr := &ConfigReloader{
		st:        st,
		fsw:       fsw,
		debounce:  500 * time.Millisecond,
		dirRefs:   map[string]int{},
		repos:     map[string]struct{}{},
		timers:    map[string]*time.Timer{},
		reloadLks: map[string]*sync.Mutex{},
	}
	if home, err := os.UserHomeDir(); err == nil {
		cr.globalDir = filepath.Join(home, ".config", "treeman")
		if err := os.MkdirAll(cr.globalDir, 0o755); err == nil {
			cr.mu.Lock()
			cr.addDirLocked(cr.globalDir)
			cr.mu.Unlock()
		}
	}
	return cr, nil
}

// Start spawns the fsnotify drain loop. The loop exits when ctx is
// cancelled — that closes the underlying watcher.
func (cr *ConfigReloader) Start(ctx context.Context) {
	safeGo(lblConfigReload, "", func() { cr.loop(ctx) })
}

// AddRepo begins watching `repoPath` for changes to `.treeman.yaml`
// and `.treeman.local.yaml`. Idempotent — a repo already tracked is a
// no-op. Called both from boot (for every row in `repos`) and from
// `startRepoWatcher` so newly-enrolled repos pick up live edits.
func (cr *ConfigReloader) AddRepo(repoPath string) {
	if cr == nil || repoPath == "" {
		return
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if _, ok := cr.repos[repoPath]; ok {
		return
	}
	cr.repos[repoPath] = struct{}{}
	cr.addDirLocked(repoPath)
}

// RemoveRepo stops watching `repoPath`'s config files. Idempotent.
func (cr *ConfigReloader) RemoveRepo(repoPath string) {
	if cr == nil || repoPath == "" {
		return
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if _, ok := cr.repos[repoPath]; !ok {
		return
	}
	delete(cr.repos, repoPath)
	cr.removeDirLocked(repoPath)
}

func (cr *ConfigReloader) addDirLocked(dir string) {
	if cr.dirRefs[dir] == 0 {
		if err := cr.fsw.Add(dir); err != nil {
			slog.Warn("config reloader: fsnotify add failed", "dir", dir, "err", err)
			return
		}
	}
	cr.dirRefs[dir]++
}

func (cr *ConfigReloader) removeDirLocked(dir string) {
	cr.dirRefs[dir]--
	if cr.dirRefs[dir] <= 0 {
		delete(cr.dirRefs, dir)
		_ = cr.fsw.Remove(dir)
	}
}

func (cr *ConfigReloader) loop(ctx context.Context) {
	defer func() { _ = cr.fsw.Close() }()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-cr.fsw.Events:
			if !ok {
				return
			}
			if !isInterestingConfigEvent(ev) {
				continue
			}
			cr.scheduleReload(filepath.Dir(ev.Name))
		case err, ok := <-cr.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("config reloader: fsnotify error", "err", err)
		}
	}
}

// isInterestingConfigEvent filters fsnotify events down to the two
// per-repo YAML files and the global config file. Chmod and rename-
// of-bak files don't trigger reloads.
func isInterestingConfigEvent(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
		return false
	}
	base := filepath.Base(ev.Name)
	switch base {
	case ".treeman.yaml", ".treeman.local.yaml", "config.yaml":
		return true
	}
	return false
}

// scheduleReload debounces a reload per scope. `dir` is either the
// global config dir (→ reload all repos) or a tracked repo path (→
// reload just that repo).
func (cr *ConfigReloader) scheduleReload(dir string) {
	cr.mu.Lock()
	key := ""
	isGlobal := dir == cr.globalDir && cr.globalDir != ""
	if !isGlobal {
		if _, ok := cr.repos[dir]; !ok {
			cr.mu.Unlock()
			return
		}
		key = dir
	}
	cr.mu.Unlock()

	cr.timersMu.Lock()
	if t, ok := cr.timers[key]; ok {
		t.Stop()
	}
	cr.timers[key] = time.AfterFunc(cr.debounce, func() {
		cr.timersMu.Lock()
		delete(cr.timers, key)
		cr.timersMu.Unlock()
		bg := cr.st.BgCtx
		if isGlobal {
			cr.ReloadAll(bg)
		} else {
			cr.ReloadRepo(bg, key)
		}
	})
	cr.timersMu.Unlock()
}

// ReloadAll invalidates the resolved-config cache and reloads watchers
// for every registered repo. Invoked by the global-dir fsnotify path
// and by SIGHUP / `config_reload` RPC with empty repo path.
func (cr *ConfigReloader) ReloadAll(ctx context.Context) {
	resolve.InvalidateConfigCache()
	paths, err := cr.st.Store.ListRepoPaths(ctx)
	if err != nil {
		slog.Warn("config reload: list repos failed", "err", err)
		return
	}
	for _, p := range paths {
		cr.reloadOne(ctx, p)
	}
	// Re-read the global `notifications:` block so toggling it on/off
	// (or changing the bucket list / backend) in
	// `~/.config/treeman/config.yaml` takes effect live, without a
	// daemon restart. Lives here (not reloadOne) because notifications
	// are global-only and ReloadAll is the global-config seam.
	RegisterNotifier(cr.st)
	_ = cr.st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtConfigReload,
		"config reload restarted watchers (all repos)", 0, 0, "", 0,
		map[string]string{"scope": "all"})
}

// ReloadRepo invalidates the resolved-config cache and reloads the
// per-repo watchers for one repo.
func (cr *ConfigReloader) ReloadRepo(ctx context.Context, repoPath string) {
	resolve.InvalidateConfigCache()
	cr.reloadOne(ctx, repoPath)
	_ = cr.st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtConfigReload,
		"config reload restarted watchers", 0, 0, "", 0,
		map[string]string{"repo": repoPath})
}

// reloadOne cancels every live watcher attached to `repoPath` and then
// re-runs the boot-time spawn paths. The cancel order matters: the
// lifecycle watcher lives on the repo entry, the per-worktree fsnotify
// watchers live on each worktree path. Killing them all first means
// `startRepoWatcher` / `startWorktreeWatcher` can run their normal
// "skip when already registered" guards without leaking goroutines.
func (cr *ConfigReloader) reloadOne(ctx context.Context, repoPath string) {
	if repoPath == "" {
		return
	}
	if _, err := os.Stat(repoPath); err != nil {
		slog.Warn("config reload: repo missing", "repo", repoPath, "err", err)
		return
	}
	// Serialise per-repo. SIGHUP, the `config_reload` RPC, and the
	// debounced fsnotify timer all reach this function; without a
	// per-repo gate two reload goroutines can interleave their
	// EnrollMainWorktree / unregister / re-spawn sequences against
	// the same repo, leaking watchers or producing inconsistent
	// state. The mutex is cheap (one map lookup) and only blocks
	// reloads of the same repo — different repos still parallelise.
	mu := cr.lockRepo(repoPath)
	mu.Lock()
	defer mu.Unlock()

	slog.Info("config reload: restarting watchers", "repo", repoPath)

	// Re-sync the main-wt row with the freshly-loaded config BEFORE
	// listing active worktrees below. Flipping `main_worktree.enabled`
	// on inserts a new row; flipping it off soft-deletes the existing
	// one. Either change is observed by the ListActiveWorktrees call
	// that follows, so per-wt watcher spawn/teardown happens through
	// the same code path linked wts use.
	if _, err := EnrollMainWorktree(ctx, cr.st, repoPath); err != nil {
		slog.Warn("config reload: main worktree enroll", "repo", repoPath, "err", err)
	}

	cr.st.UnregisterWatcher(repoPath)
	cr.st.UnregisterLifecycleWatcher(repoPath)

	wts, err := cr.st.Store.ListActiveWorktrees(ctx)
	if err != nil {
		slog.Warn("config reload: list active worktrees failed", "err", err)
	}
	for _, wt := range wts {
		if wt.RepoPath != repoPath {
			continue
		}
		cr.st.UnregisterWtWatcher(wt.WorktreePath)
	}

	if err := startRepoWatcher(ctx, cr.st, repoPath); err != nil {
		slog.Warn("config reload: startRepoWatcher", "repo", repoPath, "err", err)
	}
	for _, wt := range wts {
		if wt.RepoPath != repoPath {
			continue
		}
		if _, err := os.Stat(wt.WorktreePath); err != nil {
			continue
		}
		if err := startWorktreeWatcher(ctx, cr.st, repoPath, wt.WorktreePath); err != nil {
			slog.Warn("config reload: startWorktreeWatcher",
				"wt", wt.WorktreePath, "err", err)
		}
	}
}

// lockRepo returns the per-repo reload mutex, lazily creating it on
// first use. Callers Lock() on entry and Unlock() on exit. Map-level
// access is guarded by reloadLksMu; the returned per-repo mutex is
// what serialises actual reload work.
func (cr *ConfigReloader) lockRepo(repoPath string) *sync.Mutex {
	cr.reloadLksMu.Lock()
	defer cr.reloadLksMu.Unlock()
	mu, ok := cr.reloadLks[repoPath]
	if !ok {
		mu = &sync.Mutex{}
		cr.reloadLks[repoPath] = mu
	}
	return mu
}
