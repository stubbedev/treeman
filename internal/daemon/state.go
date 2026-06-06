package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/stubbedev/treeman/internal/config"
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

	// SyncFinalize forces worktree-create's hooks+prepare tail to run
	// synchronously instead of in a detached goroutine. The real daemon
	// leaves this false (fast return; the tail outlives the RPC). The
	// in-process executor (RunPlanInProcess, used by the CLI/MCP when no
	// daemon is reachable) sets it true so the tail completes before the
	// ephemeral State + store are torn down.
	SyncFinalize bool

	mu                sync.Mutex
	watchers          map[string]*WatcherEntry
	wtWatchers        map[string]*WatcherEntry
	lifecycleWatchers map[string]*WatcherEntry

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
	//
	// inFlightFinalizes maps wtPath → cancel func of the FinalizeWorktree
	// goroutine running for that path. Teardown calls CancelFinalize
	// before its own work so a late-arriving prepare can't resurrect
	// state (DB + registry row) after the worktree has been deleted.
	inFlightMu        sync.Mutex
	inFlightTeardowns map[string]struct{}
	inFlightFinalizes map[string]inFlightFinalize

	// reapQueuesMu guards reapQueues. Each repo gets a single worker
	// goroutine draining a buffered channel of trash paths — bursty
	// `gwtd` runs that delete five worktrees at once don't spawn five
	// parallel reapers competing for the same disk. The worker runs
	// for the daemon's lifetime; entries are append-only.
	reapQueuesMu sync.Mutex
	reapQueues   map[string]chan string

	// dropQueues mirrors reapQueues for DROP DATABASE work. Each repo
	// has a single drain goroutine — sequential DROP per repo avoids
	// hammering the DB server when five worktrees are torn down at
	// once, while different repos can still drop in parallel.
	dropQueuesMu sync.Mutex
	dropQueues   map[string]chan DBDropJob

	// ConfigReloader watches `.treeman.yaml` + layered config files
	// and restarts per-repo watchers when they change. Initialised by
	// `treemand` main; nil in unit tests that exercise dispatch
	// without booting the full daemon.
	ConfigReloader *ConfigReloader

	// syncMu guards the sync-state maps below. The auto-fetch loop and
	// on-demand sync RPCs both mutate this from different goroutines;
	// a single mutex is cheaper than per-repo locks given the small
	// number of registered repos.
	syncMu sync.Mutex
	// syncBackoff[repoPath] is the wall time at which the repo becomes
	// eligible for another auto-fetch. Set after a fetch failure;
	// cleared on success. Manual `sync_now` ignores this entirely so
	// the user can override an offline-mode pause.
	syncBackoff map[string]time.Time
	// syncFailCount[repoPath] is the consecutive-failure count used to
	// derive the next backoff window (exponential, capped at 1h).
	syncFailCount map[string]int
	// syncLastFetch[repoPath] is the unix time of the most recent
	// successful `git fetch` for the repo. Read by `sync_status` so
	// callers see staleness without scanning event history.
	syncLastFetch map[string]time.Time
	// syncLastSkip[wtPath] is the most recent skip reason emitted for
	// the worktree (dirty, no-upstream, non-ff, detached). Surfaces
	// via `sync_status` for "why didn't my branch advance?".
	syncLastSkip map[string]string
}

// inFlightFinalize is the per-path tracking record kept in
// State.inFlightFinalizes. cancel preempts the FinalizeWorktree
// goroutine via its context; startedAt anchors the watchdog that
// fails-out hung finalizes whose owning child has wedged (see
// WatchdogStalePreparing).
type inFlightFinalize struct {
	cancel    context.CancelFunc
	startedAt time.Time
}

// DBDropJob is one queued `prepare.TeardownDatabases` invocation,
// scheduled by TeardownWorktree to run in the background after
// teardown hooks complete. The cfg field is a value copy — the
// caller's mutation cannot race the drain worker.
type DBDropJob struct {
	Cfg        config.Config
	Slug       string
	RepoID     int64
	WorktreeID int64
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
		PID:               uint32(syscallPid()), //nolint:gosec // a PID is non-negative and fits uint32
		BgCtx:             bg,
		watchers:          map[string]*WatcherEntry{},
		wtWatchers:        map[string]*WatcherEntry{},
		lifecycleWatchers: map[string]*WatcherEntry{},
		teardownLks:       map[string]*sync.Mutex{},
		inFlightTeardowns: map[string]struct{}{},
		inFlightFinalizes: map[string]inFlightFinalize{},
		reapQueues:        map[string]chan string{},
		dropQueues:        map[string]chan DBDropJob{},
		syncBackoff:       map[string]time.Time{},
		syncFailCount:     map[string]int{},
		syncLastFetch:     map[string]time.Time{},
		syncLastSkip:      map[string]string{},
	}
}

// SyncBackoffUntil returns the wall time before which the repo should
// be skipped by the auto-fetch sweep. Zero value means "eligible now".
func (st *State) SyncBackoffUntil(repoPath string) time.Time {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	return st.syncBackoff[repoPath]
}

// RecordSyncFailure bumps the consecutive-failure count for repoPath
// and parks the next-eligible time to `now + delay`. `delay` is
// computed by the caller (lets the auto-fetch loop reuse the same
// helper for variable backoff curves). Returns the new fail count.
func (st *State) RecordSyncFailure(repoPath string, delay time.Duration) int {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	st.syncFailCount[repoPath]++
	st.syncBackoff[repoPath] = time.Now().Add(delay)
	return st.syncFailCount[repoPath]
}

// RecordSyncSuccess clears any backoff entry for repoPath and stamps
// the last-successful-fetch time. Called after a fetch returns nil.
func (st *State) RecordSyncSuccess(repoPath string) {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	delete(st.syncBackoff, repoPath)
	delete(st.syncFailCount, repoPath)
	st.syncLastFetch[repoPath] = time.Now()
}

// SyncFailCount returns the consecutive-failure count for repoPath.
// 0 means the last attempt succeeded (or no attempt yet).
func (st *State) SyncFailCount(repoPath string) int {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	return st.syncFailCount[repoPath]
}

// SyncLastFetch returns the wall time of the last successful fetch for
// repoPath. Zero value when no fetch has succeeded since daemon boot.
func (st *State) SyncLastFetch(repoPath string) time.Time {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	return st.syncLastFetch[repoPath]
}

// RecordSyncSkip stores the most recent skip reason for a worktree so
// the sync_status RPC can surface "why didn't my branch advance?"
// without scanning event history.
func (st *State) RecordSyncSkip(wtPath, reason string) {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	if reason == "" {
		delete(st.syncLastSkip, wtPath)
		return
	}
	st.syncLastSkip[wtPath] = reason
}

// SyncLastSkip returns the most recent skip reason for wtPath, or ""
// if the last advance succeeded (or never ran).
func (st *State) SyncLastSkip(wtPath string) string {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	return st.syncLastSkip[wtPath]
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

// MarkFinalizeInFlight records that a FinalizeWorktree goroutine is
// running for wtPath and stores its cancel func so a concurrent
// teardown can preempt it. Returns false when another finalize is
// already in flight for the same wtPath — caller should return
// early to avoid double-firing setup hooks. The caller MUST
// invoke UnmarkFinalizeInFlight on exit.
//
// Why this exists: when the lifecycle watcher is enabled, the
// `git worktree add` shelled out by `treeman wt create` itself
// generates an fsnotify CREATE event that the watcher debounces
// and then dispatches FinalizeWorktree against. Without dedup, the
// CLI-initiated finalize and the watcher-initiated finalize run in
// parallel — both fire setup hooks (composer install, npm
// install, …) and both call prepare, fighting each other on file
// locks.
func (st *State) MarkFinalizeInFlight(wtPath string, cancel context.CancelFunc) bool {
	st.inFlightMu.Lock()
	defer st.inFlightMu.Unlock()
	if _, exists := st.inFlightFinalizes[wtPath]; exists {
		return false
	}
	if cancel == nil {
		cancel = func() {}
	}
	st.inFlightFinalizes[wtPath] = inFlightFinalize{cancel: cancel, startedAt: time.Now()}
	return true
}

// UnmarkFinalizeInFlight clears the in-flight marker for wtPath.
func (st *State) UnmarkFinalizeInFlight(wtPath string) {
	st.inFlightMu.Lock()
	delete(st.inFlightFinalizes, wtPath)
	st.inFlightMu.Unlock()
}

// IsFinalizeInFlight reports whether a FinalizeWorktree is
// currently running for wtPath. The lifecycle watcher uses this
// (alongside the active-row check) to suppress its own dispatch
// when the CLI is already handling the create.
func (st *State) IsFinalizeInFlight(wtPath string) bool {
	st.inFlightMu.Lock()
	_, ok := st.inFlightFinalizes[wtPath]
	st.inFlightMu.Unlock()
	return ok
}

// CancelFinalize fires the cancel func registered for wtPath if a
// FinalizeWorktree is currently in flight. Idempotent — safe to call
// when no finalize is running. Returns true when a cancel was fired.
//
// Called by TeardownWorktree to preempt any in-flight finalize so
// `prepare.Run` doesn't race the cleanup and resurrect databases
// after the worktree has been removed.
func (st *State) CancelFinalize(wtPath string) bool {
	st.inFlightMu.Lock()
	entry, ok := st.inFlightFinalizes[wtPath]
	st.inFlightMu.Unlock()
	if !ok || entry.cancel == nil {
		return false
	}
	entry.cancel()
	return true
}

// SnapshotInFlightFinalizes returns a copy of (wtPath, startedAt) for
// every FinalizeWorktree currently running. Used by the watchdog to
// detect finalizes whose owning child has wedged past the prepare
// timeout. Copying the map (rather than handing back the live one)
// keeps the inFlightMu critical section short.
func (st *State) SnapshotInFlightFinalizes() map[string]time.Time {
	st.inFlightMu.Lock()
	defer st.inFlightMu.Unlock()
	out := make(map[string]time.Time, len(st.inFlightFinalizes))
	for p, e := range st.inFlightFinalizes {
		out[p] = e.startedAt
	}
	return out
}

// WaitFinalizeCleared blocks until no FinalizeWorktree is in flight
// for wtPath or `timeout` elapses. Returns an error when the
// finalize slot is still occupied after the deadline. Used by
// TeardownWorktree to serialise after a cancelled finalize so the
// cleanup observes a stable, post-finalize state of the DB and
// registry.
func (st *State) WaitFinalizeCleared(ctx context.Context, wtPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !st.IsFinalizeInFlight(wtPath) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("finalize still in flight for %s after %s", wtPath, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
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
	return uint32(len(st.watchers)) //nolint:gosec // watcher count is small and non-negative
}

// RegisterWatcher inserts (or replaces) an entry; Phase 10 wires the
// actual fsnotify spawn. The prior entry's Cancel runs after the map
// mutation and the mutex are released so watcher-goroutine cleanup
// callbacks that try to take st.mu (e.g. self-unregistering on exit)
// can't deadlock.
func (st *State) RegisterWatcher(repoPath string, e *WatcherEntry) {
	st.mu.Lock()
	prev := st.watchers[repoPath]
	st.watchers[repoPath] = e
	st.mu.Unlock()
	if prev != nil && prev.Cancel != nil {
		prev.Cancel()
	}
}

// UnregisterWatcher cancels + drops a watcher entry. Cancel runs after
// the mutex is released (see RegisterWatcher).
func (st *State) UnregisterWatcher(repoPath string) bool {
	st.mu.Lock()
	e, ok := st.watchers[repoPath]
	if !ok {
		st.mu.Unlock()
		return false
	}
	delete(st.watchers, repoPath)
	st.mu.Unlock()
	if e.Cancel != nil {
		e.Cancel()
	}
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
	prev := st.wtWatchers[wtPath]
	st.wtWatchers[wtPath] = e
	st.mu.Unlock()
	if prev != nil && prev.Cancel != nil {
		prev.Cancel()
	}
}

// UnregisterWtWatcher cancels + drops a per-worktree watcher.
func (st *State) UnregisterWtWatcher(wtPath string) bool {
	st.mu.Lock()
	e, ok := st.wtWatchers[wtPath]
	if !ok {
		st.mu.Unlock()
		return false
	}
	delete(st.wtWatchers, wtPath)
	st.mu.Unlock()
	if e.Cancel != nil {
		e.Cancel()
	}
	return true
}

// RegisterLifecycleWatcher inserts (or replaces) a lifecycle-watcher
// entry for repoPath, cancelling any prior watcher for the same path.
func (st *State) RegisterLifecycleWatcher(repoPath string, e *WatcherEntry) {
	st.mu.Lock()
	prev := st.lifecycleWatchers[repoPath]
	st.lifecycleWatchers[repoPath] = e
	st.mu.Unlock()
	if prev != nil && prev.Cancel != nil {
		prev.Cancel()
	}
}

// UnregisterLifecycleWatcher cancels + drops the lifecycle watcher
// entry for repoPath.
func (st *State) UnregisterLifecycleWatcher(repoPath string) bool {
	st.mu.Lock()
	e, ok := st.lifecycleWatchers[repoPath]
	if !ok {
		st.mu.Unlock()
		return false
	}
	delete(st.lifecycleWatchers, repoPath)
	st.mu.Unlock()
	if e.Cancel != nil {
		e.Cancel()
	}
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

// ListWtWatcherPaths returns the registered per-worktree watcher paths.
// Used by daemon_state to surface which worktrees are observed.
func (st *State) ListWtWatcherPaths() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.wtWatchers))
	for p := range st.wtWatchers {
		out = append(out, p)
	}
	return out
}

// ListLifecycleWatcherPaths returns the repo paths currently
// covered by a lifecycle watcher.
func (st *State) ListLifecycleWatcherPaths() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]string, 0, len(st.lifecycleWatchers))
	for p := range st.lifecycleWatchers {
		out = append(out, p)
	}
	return out
}

// SnapshotInFlightTeardowns returns the set of worktree paths whose
// primary TeardownWorktree is currently running.
func (st *State) SnapshotInFlightTeardowns() []string {
	st.inFlightMu.Lock()
	defer st.inFlightMu.Unlock()
	out := make([]string, 0, len(st.inFlightTeardowns))
	for p := range st.inFlightTeardowns {
		out = append(out, p)
	}
	return out
}

// SnapshotSyncBackoffs returns a copy of (repoPath, consec_failures,
// next_retry_unix) for every repo currently in backoff. Entries with
// no backoff window are omitted.
func (st *State) SnapshotSyncBackoffs() map[string]struct {
	Failures      int
	NextRetryUnix int64
} {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	out := make(map[string]struct {
		Failures      int
		NextRetryUnix int64
	}, len(st.syncBackoff))
	for repo, t := range st.syncBackoff {
		out[repo] = struct {
			Failures      int
			NextRetryUnix int64
		}{
			Failures:      st.syncFailCount[repo],
			NextRetryUnix: t.Unix(),
		}
	}
	return out
}

// SnapshotSyncLastSkips returns a copy of the most-recent skip reason
// per worktree (e.g. dirty, no-upstream). Read by daemon_state.
func (st *State) SnapshotSyncLastSkips() map[string]string {
	st.syncMu.Lock()
	defer st.syncMu.Unlock()
	out := make(map[string]string, len(st.syncLastSkip))
	maps.Copy(out, st.syncLastSkip)
	return out
}

// WatcherSummary mirrors `rpc.WatcherSummary` — duplicated to keep
// the dependency direction clean (daemon → store, not daemon → rpc).
type WatcherSummary struct {
	Repo          string
	WorktreeCount uint32
}
