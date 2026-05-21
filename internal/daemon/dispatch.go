package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/binlog"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/template"
	"github.com/stubbedev/treeman/internal/version"
)

// Dispatch executes one RPC request against the live state, returning
// the response to send back over the socket. Mirrors the `dispatch`
// fn in `crates/treeman-daemon/src/main.rs`.
//
// The shutdown channel is closed when a `Shutdown` request fires,
// signalling the main loop to bail.
func Dispatch(ctx context.Context, st *State, shutdown chan<- struct{}, req rpc.Request) rpc.Response {
	switch req.Method {
	case rpc.MethodPing:
		return rpc.Response{Kind: rpc.KindPong}

	case rpc.MethodStatus:
		return rpc.Response{
			Kind:            rpc.KindStatus,
			ProtocolVersion: rpc.ProtocolVersion,
			DaemonVersion:   version.Version,
			Pid:             st.PID,
			StartedAtUnix:   st.StartedAtUnix,
			WatcherCount:    st.WatcherCount(),
		}

	case rpc.MethodShutdown:
		select {
		case shutdown <- struct{}{}:
		default:
		}
		return rpc.Response{Kind: rpc.KindOk}

	case rpc.MethodRepoRegister:
		if req.RepoRegister == nil {
			return errResp("repo_register: missing args")
		}
		id, err := st.Store.EnsureRepo(ctx, req.RepoRegister.Path, req.RepoRegister.Name)
		if err != nil {
			return errResp(err.Error())
		}
		return rpc.Response{Kind: rpc.KindRepoRegistered, RepoID: id}

	case rpc.MethodWatcherList:
		s := st.ListWatchers()
		out := make([]rpc.WatcherSummary, len(s))
		for i, e := range s {
			out[i] = rpc.WatcherSummary{Repo: e.Repo, WorktreeCount: e.WorktreeCount}
		}
		return rpc.Response{Kind: rpc.KindWatcherList, Repos: out}

	case rpc.MethodWorktreeFinalize:
		if req.WorktreeFinalize == nil {
			return errResp("worktree_finalize: missing args")
		}
		args := *req.WorktreeFinalize
		go func() {
			err := FinalizeWorktree(context.Background(), st, args.RepoPath, args.WorktreePath, args.InheritedEnv)
			if err != nil {
				_ = st.Store.WriteEvent(context.Background(), "error", "wt_finalize", err.Error(),
					0, 0, "", 0, map[string]string{
						"repo_path": args.RepoPath, "worktree_path": args.WorktreePath,
					})
			}
		}()
		return rpc.Response{Kind: rpc.KindWorktreeFinalizeQueued, WorktreePath: args.WorktreePath}

	case rpc.MethodWorktreeTeardown:
		if req.WorktreeTeardown == nil {
			return errResp("worktree_teardown: missing args")
		}
		args := *req.WorktreeTeardown
		go func() {
			err := TeardownWorktree(context.Background(), st, args.RepoPath, args.WorktreePath, args.Force, args.InheritedEnv)
			if err != nil {
				_ = st.Store.WriteEvent(context.Background(), "error", "wt_teardown", err.Error(),
					0, 0, "", 0, map[string]string{
						"repo_path": args.RepoPath, "worktree_path": args.WorktreePath,
					})
			}
		}()
		return rpc.Response{Kind: rpc.KindWorktreeTeardownQueued, WorktreePath: args.WorktreePath}

	case rpc.MethodWatcherStart:
		if req.WatcherStart == nil {
			return errResp("watcher_start: missing args")
		}
		if err := startRepoWatcher(ctx, st, req.WatcherStart.RepoPath); err != nil {
			return errResp(err.Error())
		}
		return rpc.Response{Kind: rpc.KindOk}

	case rpc.MethodWatcherStop:
		if req.WatcherStop == nil {
			return errResp("watcher_stop: missing args")
		}
		st.UnregisterWatcher(req.WatcherStop.RepoPath)
		return rpc.Response{Kind: rpc.KindOk}

	case rpc.MethodWorktreeList:
		// fsnotify-based filesystem watcher is the deferred bit;
		// the daemon-side worktree listing reads SQLite directly so
		// this can land independently. Surface a clear marker.
		return errResp("worktree_list RPC not yet wired (post-v1.0)")

	default:
		return errResp("unknown method: " + req.Method)
	}
}

func phaseFor(method string) string {
	switch method {
	case rpc.MethodWatcherStart, rpc.MethodWatcherStop, rpc.MethodWorktreeList:
		return "10 (watcher)"
	case rpc.MethodWorktreeFinalize, rpc.MethodWorktreeTeardown:
		return "8/10 (prepare + daemon supervisor)"
	}
	return "TBD"
}

func errResp(msg string) rpc.Response {
	return rpc.Response{Kind: rpc.KindError, Message: msg}
}

// startRepoWatcher boots one binlog.Replicator goroutine per MySQL
// source database declared in the repo's .treeman.yaml — if
// `watcher.binlog.enabled = true`. Filesystem-event watching (the
// fsnotify side that triggers `prepare` on migration edits) is the
// deferred bit; the binlog tail covers the high-value case where
// migrations are applied to the source DB outside treeman.
//
// Each replicator runs until the WatcherEntry's cancel is invoked
// or the daemon shuts down. State tracking lets `watcher_list` /
// `status` report the running count.
func startRepoWatcher(ctx context.Context, st *State, repoPath string) error {
	if repoPath == "" {
		return fmt.Errorf("watcher_start: empty repo_path")
	}
	cfg, err := config.LoadLayered(repoPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}

	wctx, cancel := context.WithCancel(context.Background())
	entry := &WatcherEntry{
		RepoPath:      repoPath,
		WorktreeCount: 0,
		Cancel:        cancel,
	}
	st.RegisterWatcher(repoPath, entry)

	if cfg.Watcher.Binlog.Enabled {
		spawned := 0
		for _, d := range cfg.Databases {
			switch d.Engine {
			case "mysql", "mariadb", "tidb":
			default:
				continue
			}
			sourceDB, err := template.Render(d.NameTemplate, template.Context{})
			if err != nil {
				// Without a slug context the source name has
				// unresolved {slug} placeholders — that's per-
				// worktree, not a global source. Skip it; the binlog
				// tail only follows non-templated source DBs.
				continue
			}
			r, err := binlog.New(&cfg, st.Store, repoID, sourceDB)
			if err != nil {
				slog.Warn("binlog replicator init", "repo", repoPath, "source_db", sourceDB, "err", err)
				continue
			}
			go func(rep *binlog.Replicator, src string) {
				defer rep.Stop()
				if err := rep.Start(wctx); err != nil {
					slog.Warn("binlog replicator exit", "repo", repoPath, "source_db", src, "err", err)
				}
			}(r, sourceDB)
			spawned++
		}
		slog.Info("watcher started", "repo", repoPath, "binlog_replicators", spawned)
	} else {
		slog.Info("watcher started (binlog disabled)", "repo", repoPath)
	}
	return nil
}
