package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

// State holds everything the RPC dispatch needs: the SQLite store,
// the watcher registry (one entry per registered repo + one entry
// per active worktree), startup metadata. Threaded into RPC handlers
// as a pointer.
type State struct {
	Store         *store.Store
	StartedAtUnix int64
	PID           uint32

	// BgCtx is the daemon-lifetime context. Async work spawned by
	// RPC handlers (finalize, teardown, watcher dispatch) must use
	// this — not the per-request ctx, which dies when the client
	// connection closes, and not context.Background(), which
	// orphans the work on shutdown.
	BgCtx context.Context

	mu                 sync.Mutex
	watchers           map[string]*WatcherEntry
	wtWatchers         map[string]*WatcherEntry
	lifecycleWatchers  map[string]*WatcherEntry

	// teardownMu serialises wt-teardown goroutines per repo so two
	// fast-fire teardowns on the same repo can't run concurrent
	// `git worktree remove --force` + parallel DROP DATABASE storms
	// — that combination on a Laravel-sized vendor/ + node_modules/
	// (~250k file unlinks) saturated the I/O queue badly enough to
	// require a forced shutdown in field testing.
	teardownMu  sync.Mutex
	teardownLks map[string]*sync.Mutex

	// inFlightTeardowns is the set of worktree paths whose primary
	// TeardownWorktree is currently running. The lifecycle watcher's
	// onRemove consults this so the fsnotify REMOVE event fired by
	// `git worktree remove` doesn't spawn a duplicate orphan teardown
	// behind the primary's mutex.
	inFlightMu        sync.Mutex
	inFlightTeardowns map[string]struct{}
}

// WatcherEntry — per-repo watcher placeholder. Phase 10 fills in the
// actual fsnotify guts; for now this carries cancel + worktree
// counts so the RPC layer can answer `WatcherList`.
type WatcherEntry struct {
	RepoPath      string
	WorktreeCount uint32
	Cancel        context.CancelFunc
}

// NewState constructs a fresh daemon state. `bg` is the
// daemon-lifetime context — cancelled on signal/shutdown — that
// background goroutines should derive from.
func NewState(bg context.Context, s *store.Store) *State {
	if bg == nil {
		bg = context.Background()
	}
	return &State{
		Store:             s,
		StartedAtUnix:     time.Now().Unix(),
		PID:               uint32(syscallPid()),
		BgCtx:             bg,
		watchers:          map[string]*WatcherEntry{},
		wtWatchers:        map[string]*WatcherEntry{},
		lifecycleWatchers: map[string]*WatcherEntry{},
		teardownLks:       map[string]*sync.Mutex{},
		inFlightTeardowns: map[string]struct{}{},
	}
}

// MarkTeardownInFlight records that a primary TeardownWorktree is
// running for wtPath. Returns false when an in-flight entry already
// exists (a concurrent caller beat us). The caller must invoke
// UnmarkTeardownInFlight on exit — typically with defer.
func (st *State) MarkTeardownInFlight(wtPath string) bool {
	st.inFlightMu.Lock()
	defer st.inFlightMu.Unlock()
	if _, exists := st.inFlightTeardowns[wtPath]; exists {
		return false
	}
	st.inFlightTeardowns[wtPath] = struct{}{}
	return true
}

// UnmarkTeardownInFlight clears the in-flight marker for wtPath.
func (st *State) UnmarkTeardownInFlight(wtPath string) {
	st.inFlightMu.Lock()
	delete(st.inFlightTeardowns, wtPath)
	st.inFlightMu.Unlock()
}

// IsTeardownInFlight reports whether a primary teardown is currently
// running for wtPath. The lifecycle watcher uses this to skip
// spawning an orphan teardown for the same worktree.
func (st *State) IsTeardownInFlight(wtPath string) bool {
	st.inFlightMu.Lock()
	_, ok := st.inFlightTeardowns[wtPath]
	st.inFlightMu.Unlock()
	return ok
}

// safeGo runs fn in a new goroutine, recovering any panic so a
// runtime error in one async task can't kill the whole daemon.
// The panic is logged with the caller-supplied label.
func safeGo(label string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("daemon goroutine panic",
					"label", label, "panic", fmt.Sprint(r))
			}
		}()
		fn()
	}()
}

// LockRepoTeardown returns the per-repo teardown mutex, creating it
// on first call. Callers must `Lock()` on entry and `Unlock()` on
// exit — typically with `defer mu.Unlock()` immediately after the
// `Lock()`. Serialises every TeardownWorktree goroutine for a given
// repo so concurrent teardown invocations queue instead of fanning out
// disk + DB I/O.
func (st *State) LockRepoTeardown(repoPath string) *sync.Mutex {
	st.teardownMu.Lock()
	defer st.teardownMu.Unlock()
	mu, ok := st.teardownLks[repoPath]
	if !ok {
		mu = &sync.Mutex{}
		st.teardownLks[repoPath] = mu
	}
	return mu
}

// WatcherCount returns how many repos currently have a live watcher.
func (st *State) WatcherCount() uint32 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return uint32(len(st.watchers))
}

// RegisterWatcher inserts (or replaces) an entry; Phase 10 wires the
// actual fsnotify spawn.
func (st *State) RegisterWatcher(repoPath string, e *WatcherEntry) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if prev, ok := st.watchers[repoPath]; ok && prev.Cancel != nil {
		prev.Cancel()
	}
	st.watchers[repoPath] = e
}

// UnregisterWatcher cancels + drops a watcher entry.
func (st *State) UnregisterWatcher(repoPath string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.watchers[repoPath]
	if !ok {
		return false
	}
	if e.Cancel != nil {
		e.Cancel()
	}
	delete(st.watchers, repoPath)
	return true
}

// ListWatchers snapshots the current watcher map for the
// WatcherList RPC.
func (st *State) ListWatchers() []WatcherSummary {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]WatcherSummary, 0, len(st.watchers))
	for k, v := range st.watchers {
		out = append(out, WatcherSummary{Repo: k, WorktreeCount: v.WorktreeCount})
	}
	return out
}

// HasWtWatcher reports whether a per-worktree fsnotify watcher is
// currently registered for `wtPath`.
func (st *State) HasWtWatcher(wtPath string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.wtWatchers[wtPath]
	return ok
}

// RegisterWtWatcher inserts a per-worktree watcher entry, cancelling
// and replacing any prior one for the same path.
func (st *State) RegisterWtWatcher(wtPath string, e *WatcherEntry) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if prev, ok := st.wtWatchers[wtPath]; ok && prev.Cancel != nil {
		prev.Cancel()
	}
	st.wtWatchers[wtPath] = e
}

// UnregisterWtWatcher cancels + drops a per-worktree watcher.
func (st *State) UnregisterWtWatcher(wtPath string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.wtWatchers[wtPath]
	if !ok {
		return false
	}
	if e.Cancel != nil {
		e.Cancel()
	}
	delete(st.wtWatchers, wtPath)
	return true
}

// RegisterLifecycleWatcher inserts (or replaces) a lifecycle-watcher
// entry for repoPath, cancelling any prior watcher for the same path.
func (st *State) RegisterLifecycleWatcher(repoPath string, e *WatcherEntry) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if prev, ok := st.lifecycleWatchers[repoPath]; ok && prev.Cancel != nil {
		prev.Cancel()
	}
	st.lifecycleWatchers[repoPath] = e
}

// UnregisterLifecycleWatcher cancels + drops the lifecycle watcher
// entry for repoPath.
func (st *State) UnregisterLifecycleWatcher(repoPath string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.lifecycleWatchers[repoPath]
	if !ok {
		return false
	}
	if e.Cancel != nil {
		e.Cancel()
	}
	delete(st.lifecycleWatchers, repoPath)
	return true
}

// HasLifecycleWatcher reports whether repoPath currently has a live
// lifecycle watcher.
func (st *State) HasLifecycleWatcher(repoPath string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.lifecycleWatchers[repoPath]
	return ok
}

// WatcherSummary mirrors `rpc.WatcherSummary` — duplicated to keep
// the dependency direction clean (daemon → store, not daemon → rpc).
type WatcherSummary struct {
	Repo          string
	WorktreeCount uint32
}
