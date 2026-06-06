// Command treemand — the treeman daemon. Listens on a unix domain
// socket, dispatches RPCs, owns per-repo watchers, mirrors events
// into the SQLite event log.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/version"
)

// slogLevel parses a daemon.log_level YAML value into a slog.Level.
// Unknown values fall back to info with a stderr warning so a typo
// doesn't silently degrade visibility.
func slogLevel(raw string) slog.Level {
	switch raw {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "", "info":
		return slog.LevelInfo
	}
	fmt.Fprintf(os.Stderr, "treemand: unknown daemon.log_level %q, defaulting to info\n", raw)
	return slog.LevelInfo
}

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "--version" || os.Args[1] == "-V") {
		fmt.Printf("treemand %s\n", version.Version)
		return
	}

	level := slog.LevelInfo
	if gcfg, err := config.LoadGlobal(); err == nil {
		level = slogLevel(gcfg.Daemon.LogLevel)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("treemand exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP is a deliberate operator-driven reload trigger; we want
	// it routed to the reloader, NOT folded into the shutdown signal
	// set above. Register it independently so the default Go handler
	// (which would terminate the process) is overridden.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)

	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return fmt.Errorf("db path: %w", err)
	}
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()
	s.StartEventBatcher(ctx)
	slog.Info("treemand starting", "db", dbPath)

	sockPath, err := rpc.SocketPath()
	if err != nil {
		return err
	}
	daemon.RemoveStale(sockPath)
	if dir := filepath.Dir(sockPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", sockPath)
	if err != nil {
		return fmt.Errorf("bind %s: %w", sockPath, err)
	}
	if err := daemon.Lockdown(sockPath); err != nil {
		_ = ln.Close()
		return err
	}

	st := daemon.NewState(ctx, s)
	slog.Info("treemand listening",
		"event_type", store.EvtDaemonStart,
		"socket", sockPath,
		"pid", os.Getpid())
	_ = s.WriteEvent(ctx, store.LevelInfo, store.EvtDaemonStart, "treemand listening",
		0, 0, "", 0, map[string]string{"socket": sockPath})

	// Desktop notifications (opt-in via the global `notifications:`
	// block). Registered after the store + batcher are up so the hook
	// can fan lifecycle events out to notify-send / osascript.
	// ConfigReloader.ReloadAll re-registers it on every global-config
	// reload (fsnotify edit, SIGHUP, or config_reload RPC), so changes
	// take effect live without a restart.
	daemon.RegisterNotifier(st)

	// Config reloader. Subscribes to the global config dir
	// `~/.config/treeman/` immediately; per-repo dirs are added below
	// as we resume watchers (and by `startRepoWatcher` for new repos
	// enrolled at runtime).
	cr, err := daemon.NewConfigReloader(st)
	if err != nil {
		slog.Warn("config reloader init failed (continuing without hot-reload)", "err", err)
	} else {
		st.ConfigReloader = cr
		cr.Start(ctx)
	}

	// Reconcile any finalize that was in flight when the previous
	// daemon died — the goroutine is gone, so the row would otherwise
	// stay at derived state=preparing forever. SweepStalePreparing
	// emits a synthetic worktree:create:error error so the user sees the wedge
	// and can rerun `treeman wt finalize`. Runs BEFORE watchers come
	// up so the stale event is visible regardless of whether a fresh
	// finalize is about to fire.
	daemon.SweepStalePreparing(ctx, st)

	// Auto-resume per-repo watchers on boot. Each known repo gets
	// its watchers re-spawned via the same path the
	// watcher_start RPC takes. Failures per-repo are logged + skipped
	// — a missing or moved repo dir shouldn't abort daemon startup.
	//
	// Parallelised with a bounded worker pool: each repo load reads
	// the layered config from disk,
	// both of which dominate boot time on hosts with many registered
	// repos. 8 concurrent resumes keeps the host responsive while
	// cutting boot wall-time roughly proportionally.
	// Reap registry rows for repos whose root has vanished from disk
	// (ephemeral /tmp e2e checkouts, scratch clones) BEFORE listing
	// repos for watcher resume, so dead repos neither get watchers nor
	// drag their events / hook_runs / snapshots rows forever.
	daemon.ReapDeadRepos(ctx, st)

	repoPaths, _ := s.ListRepoPaths(ctx)
	resumeRepoWatchers(ctx, st, repoPaths)

	// Reap any `.treeman-trash/` leftovers from a previously crashed
	// daemon. The background reaper runs on st.BgCtx so it survives
	// the originating RPC but dies with the daemon — meaning a hard
	// host crash mid-reap orphans the trash entries until next boot.
	// Sweep on startup so they don't accumulate indefinitely.
	daemon.SweepTrashDirs(ctx, st, repoPaths)

	// Reap port reservations orphaned by an interrupted teardown or a
	// pre-release binary that soft-deleted a worktree without releasing
	// its ports. Leaked rows are invisible to ListUsedPorts but still
	// blocked by the (repo_id, name, port) unique index, so without this
	// the allocator climbs out of the configured range over time.
	daemon.SweepOrphanWorktreePorts(ctx, st)

	// Enroll opt-in main worktrees BEFORE listing active worktrees so
	// the boot resume below spawns watchers against the repo root for
	// every repo where `main_worktree.enabled: true`. Idempotent —
	// flips a row's is_main + slug to match the current branch.
	enrollMainWorktrees(ctx, st, repoPaths)

	// Auto-resume per-worktree fsnotify watchers. Migrations and
	// dumps live in the worktree (each linked worktree has its own
	// branch checkout), so the file watcher is rooted there.
	resumeWorktreeWatchers(ctx, s, st)

	// Auto-resume lifecycle watchers for every registered repo. The
	// lifecycle watcher tails `<common-dir>/worktrees/` so
	// `git worktree add`/`remove` runs outside the treeman CLI still
	// fire setup / teardown. Unconditional — worktree
	// lifecycle is infrastructure, not user policy.
	resumeLifecycleWatchers(ctx, s, st)

	// Watchdog for in-flight FinalizeWorktree goroutines: cancels +
	// flags any that exceed the prepare timeout. Catches the
	// daemon-alive-but-child-wedged variant of issue #9 that
	// SweepStalePreparing (boot-only) can't cover.
	go daemon.FinalizeWatchdogLoop(st.BgCtx, st)

	// Periodic snapshot GC sweep. Runs at the cadence declared by
	// `snapshots.retention.gc_interval_minutes` (default 60); each
	// tick walks every registered repo and evicts cached templates
	// above `cap_per_repo`. Bare-bones for now — age/size sweeps
	// (MaxAgeDays, MaxTotalGb) land here later.
	go daemon.SnapshotGCLoop(ctx, st)
	go daemon.WALCheckpointLoop(ctx, st)
	// Daemon-side retention sweep — drops events / hook_runs (and
	// cascaded hook_log_chunks) older than `logs.keep_days`.
	go daemon.LogPruneLoop(ctx, st)
	// Periodic `git fetch --all --prune` + best-effort
	// `git merge --ff-only @{u}` per registered repo's worktrees.
	// Returns immediately when `auto_fetch.enabled: false` in the
	// global config.
	go daemon.AutoFetchLoop(ctx, st)

	// SIGHUP → full reload. Independent of the shutdown signal set so
	// `kill -HUP $(pgrep treemand)` is a deterministic operator knob
	// and matches Unix daemon convention.
	if cr != nil {
		go hupReloadLoop(ctx, st, cr, hupCh)
	}

	shutdown := make(chan struct{}, 1)
	go acceptLoop(ctx, ln, st, shutdown)

	select {
	case <-ctx.Done():
		slog.Info("daemon_stopped — signal received")
		_ = s.WriteEvent(ctx, store.LevelInfo, store.EvtDaemonStop, "SIGTERM/SIGINT received", 0, 0, "", 0, nil)
	case <-shutdown:
		slog.Info("daemon_stopped — shutdown rpc")
		_ = s.WriteEvent(ctx, store.LevelInfo, store.EvtDaemonStop, "shutdown requested", 0, 0, "", 0, nil)
	}
	_ = ln.Close()
	_ = os.Remove(sockPath)
	return nil
}

// resumeRepoWatchers re-spawns per-repo watchers on boot via the same
// path the watcher_start RPC takes. Failures per-repo are logged +
// skipped — a missing or moved repo dir shouldn't abort startup.
func resumeRepoWatchers(ctx context.Context, st *daemon.State, repoPaths []string) {
	parallelFor(repoPaths, 8, func(p string) {
		if _, err := os.Stat(p); err != nil {
			slog.Warn("resume watcher skipped (path missing)", "repo", p, "err", err)
			return
		}
		if err := daemon.ResumeRepoWatcher(ctx, st, p); err != nil {
			slog.Warn("resume watcher failed", "repo", p, "err", err)
			return
		}
		slog.Info("resumed watcher", "repo", p)
	})
}

// enrollMainWorktrees flips each repo's main worktree row to match the
// current branch where `main_worktree.enabled: true`. Idempotent.
func enrollMainWorktrees(ctx context.Context, st *daemon.State, repoPaths []string) {
	for _, p := range repoPaths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if _, err := daemon.EnrollMainWorktree(ctx, st, p); err != nil {
			slog.Warn("main worktree enroll skipped", "repo", p, "err", err)
		}
	}
}

// resumeWorktreeWatchers re-spawns per-worktree fsnotify watchers on
// boot. Migrations and dumps live in the worktree, so the file watcher
// is rooted there.
func resumeWorktreeWatchers(ctx context.Context, s *store.Store, st *daemon.State) {
	wts, err := s.ListActiveWorktrees(ctx)
	if err != nil {
		return
	}
	parallelFor(wts, 8, func(w store.ActiveWorktree) {
		if _, err := os.Stat(w.WorktreePath); err != nil {
			slog.Warn("resume wt watcher skipped (path missing)",
				"wt", w.WorktreePath, "err", err)
			return
		}
		if err := daemon.ResumeWorktreeWatcher(ctx, st, w.RepoPath, w.WorktreePath); err != nil {
			slog.Warn("resume wt watcher failed",
				"wt", w.WorktreePath, "err", err)
			return
		}
		slog.Info("resumed wt watcher", "wt", w.WorktreePath)
	})
}

// resumeLifecycleWatchers re-spawns lifecycle watchers for every
// registered repo so `git worktree add`/`remove` outside the treeman
// CLI still fire setup / teardown.
func resumeLifecycleWatchers(ctx context.Context, s *store.Store, st *daemon.State) {
	repos, err := s.ListRepoRefs(ctx)
	if err != nil {
		return
	}
	for _, r := range repos {
		if _, err := os.Stat(r.Path); err != nil {
			slog.Warn("resume lifecycle watcher skipped (path missing)",
				"repo", r.Path, "err", err)
			continue
		}
		if _, err := daemon.StartLifecycleWatcher(ctx, st, r.ID, r.Path); err != nil {
			slog.Warn("resume lifecycle watcher failed",
				"repo", r.Path, "err", err)
			continue
		}
	}
}

// hupReloadLoop routes SIGHUP to a full config reload until ctx is
// cancelled. Independent of the shutdown signal set so
// `kill -HUP $(pgrep treemand)` is a deterministic operator knob.
func hupReloadLoop(ctx context.Context, st *daemon.State, cr *daemon.ConfigReloader, hupCh <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hupCh:
			slog.Info("SIGHUP received — reloading config")
			// ReloadAll re-registers the desktop notifier from the
			// refreshed global config, so SIGHUP picks up
			// `notifications:` edits too.
			cr.ReloadAll(st.BgCtx)
		}
	}
}

func acceptLoop(ctx context.Context, ln net.Listener, st *daemon.State, shutdown chan struct{}) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("accept", "err", err)
			continue
		}
		if err := daemon.CheckPeerUID(conn); err != nil {
			slog.Warn("rejecting peer", "err", err)
			_ = conn.Close()
			continue
		}
		go handleConn(ctx, conn, st, shutdown)
	}
}

func handleConn(ctx context.Context, conn net.Conn, st *daemon.State, shutdown chan struct{}) {
	defer func() { _ = conn.Close() }()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req rpc.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		if daemon.IsStreamingMethod(req.Method) {
			// Streaming methods own the connection for the duration of
			// the subscription. Caller closing the socket (or our ctx
			// cancelling) ends the stream; we don't loop for more
			// requests on the same conn.
			daemon.DispatchStreaming(ctx, st, enc, req)
			return
		}
		resp := daemon.Dispatch(ctx, st, shutdown, req)
		if err := enc.Encode(&resp); err != nil {
			return
		}
	}
}

// parallelFor runs fn over every element of items concurrently with
// at most `workers` in flight. Bounded so a host with hundreds of
// registered repos doesn't fork-bomb itself during boot.
func parallelFor[T any](items []T, workers int, fn func(T)) {
	if len(items) == 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, it := range items {
		sem <- struct{}{}
		wg.Add(1)
		go func(v T) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(v)
		}(it)
	}
	wg.Wait()
}
