// Command treemand — the treeman daemon. Listens on a unix domain
// socket, dispatches RPCs, owns per-repo watchers, mirrors events
// into the SQLite event log.
//
// Ported from `crates/treeman-daemon/src/main.rs`. The Rust
// implementation is the spec.
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
		_ = os.MkdirAll(dir, 0o755)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("bind %s: %w", sockPath, err)
	}
	if err := daemon.Lockdown(sockPath); err != nil {
		return err
	}

	st := daemon.NewState(s)
	slog.Info("treemand listening",
		"event_type", "daemon_started",
		"socket", sockPath,
		"pid", os.Getpid())
	_ = s.WriteEvent(ctx, store.LevelInfo, "daemon_started", "treemand listening",
		0, 0, "", 0, map[string]string{"socket": sockPath})

	// Auto-resume per-repo watchers on boot. The Rust daemon's
	// state.rs has the full fsnotify wiring; Phase 10 lights that
	// up. For now we just log the repos we'd resume.
	if paths, err := s.ListRepoPaths(ctx); err == nil {
		for _, p := range paths {
			slog.Info("would resume watcher (phase 10 wires fsnotify)", "repo", p)
		}
	}

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
