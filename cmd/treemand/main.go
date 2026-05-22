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
	"syscall"

	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/version"
)

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "--version" || os.Args[1] == "-V") {
		fmt.Printf("treemand %s\n", version.Version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("treemand exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return fmt.Errorf("db path: %w", err)
	}
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()
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
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("bind %s: %w", sockPath, err)
	}
	if err := daemon.Lockdown(sockPath); err != nil {
		_ = ln.Close()
		return err
	}

	st := daemon.NewState(ctx, s)
	slog.Info("treemand listening",
		"event_type", "daemon_started",
		"socket", sockPath,
		"pid", os.Getpid())
	_ = s.WriteEvent(ctx, store.LevelInfo, "daemon_started", "treemand listening",
		0, 0, "", 0, map[string]string{"socket": sockPath})

	// Auto-resume per-repo watchers on boot. Each known repo gets
	// its binlog replicators re-spawned via the same path the
	// watcher_start RPC takes. Failures per-repo are logged + skipped
	// — a missing or moved repo dir shouldn't abort daemon startup.
	if paths, err := s.ListRepoPaths(ctx); err == nil {
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				slog.Warn("resume watcher skipped (path missing)", "repo", p, "err", err)
				continue
			}
			if err := daemon.ResumeRepoWatcher(ctx, st, p); err != nil {
				slog.Warn("resume watcher failed", "repo", p, "err", err)
				continue
			}
			slog.Info("resumed watcher", "repo", p)
		}
	}

	// Auto-resume per-worktree fsnotify watchers. Migrations and
	// dumps live in the worktree (each linked worktree has its own
	// branch checkout), so the file watcher is rooted there.
	if wts, err := s.ListActiveWorktrees(ctx); err == nil {
		for _, w := range wts {
			if _, err := os.Stat(w.WorktreePath); err != nil {
				slog.Warn("resume wt watcher skipped (path missing)",
					"wt", w.WorktreePath, "err", err)
				continue
			}
			if err := daemon.ResumeWorktreeWatcher(ctx, st, w.RepoPath, w.WorktreePath); err != nil {
				slog.Warn("resume wt watcher failed",
					"wt", w.WorktreePath, "err", err)
				continue
			}
			slog.Info("resumed wt watcher", "wt", w.WorktreePath)
		}
	}

	// Auto-resume lifecycle watchers for repos that opted in via
	// `treeman wt watch on`. The lifecycle watcher tails
	// `<common-dir>/worktrees/` so `git worktree add`/`remove` run
	// outside the treeman CLI still fire postcreate / postdelete.
	// Gated on both the per-repo opt-in row and the resolved config
	// bool `worktrees.hook_lifecycle`.
	if repos, err := s.ListLifecycleWatchedRepos(ctx); err == nil {
		for _, r := range repos {
			if _, err := os.Stat(r.Path); err != nil {
				slog.Warn("resume lifecycle watcher skipped (path missing)",
					"repo", r.Path, "err", err)
				continue
			}
			if !daemon.LifecycleEnabledForRepo(r.Path) {
				slog.Info("lifecycle watcher skipped (config disabled)", "repo", r.Path)
				continue
			}
			if _, err := daemon.StartLifecycleWatcher(ctx, st, r.ID, r.Path); err != nil {
				slog.Warn("resume lifecycle watcher failed",
					"repo", r.Path, "err", err)
				continue
			}
		}
	}

	// Periodic snapshot GC sweep. Runs at the cadence declared by
	// `snapshots.retention.gc_interval_minutes` (default 60); each
	// tick walks every registered repo and evicts cached templates
	// above `cap_per_repo`. Bare-bones for now — age/size sweeps
	// (MaxAgeDays, MaxTotalGb) land here later.
	go daemon.SnapshotGCLoop(ctx, st)

	shutdown := make(chan struct{}, 1)
	go acceptLoop(ctx, ln, st, shutdown)

	select {
	case <-ctx.Done():
		slog.Info("daemon_stopped — signal received")
		_ = s.WriteEvent(ctx, store.LevelInfo, "daemon_stopped", "SIGTERM/SIGINT received", 0, 0, "", 0, nil)
	case <-shutdown:
		slog.Info("daemon_stopped — shutdown rpc")
		_ = s.WriteEvent(ctx, store.LevelInfo, "daemon_stopped", "shutdown requested", 0, 0, "", 0, nil)
	}
	_ = ln.Close()
	_ = os.Remove(sockPath)
	return nil
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
			conn.Close()
			continue
		}
		go handleConn(ctx, conn, st, shutdown)
	}
}

func handleConn(ctx context.Context, conn net.Conn, st *daemon.State, shutdown chan struct{}) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req rpc.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		resp := daemon.Dispatch(ctx, st, shutdown, req)
		if err := enc.Encode(&resp); err != nil {
			return
		}
	}
}
