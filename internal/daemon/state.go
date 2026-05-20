package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

// State holds everything the RPC dispatch needs: the SQLite store,
// the watcher registry (one entry per registered repo), startup
// metadata. Equivalent to `crates/treeman-daemon/src/state.rs`'s
// `DaemonState`. Threaded into RPC handlers as a pointer.
type State struct {
	Store         *store.Store
	StartedAtUnix int64
	PID           uint32

	mu       sync.Mutex
	watchers map[string]*WatcherEntry
}

// WatcherEntry — per-repo watcher placeholder. Phase 10 fills in the
// actual fsnotify guts; for now this carries cancel + worktree
// counts so the RPC layer can answer `WatcherList`.
type WatcherEntry struct {
	RepoPath      string
	WorktreeCount uint32
	Cancel        context.CancelFunc
}

// NewState constructs a fresh daemon state.
func NewState(s *store.Store) *State {
	return &State{
		Store:         s,
		StartedAtUnix: time.Now().Unix(),
		PID:           uint32(syscallPid()),
		watchers:      map[string]*WatcherEntry{},
	}
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

// WatcherSummary mirrors `rpc.WatcherSummary` — duplicated to keep
// the dependency direction clean (daemon → store, not daemon → rpc).
type WatcherSummary struct {
	Repo          string
	WorktreeCount uint32
}
