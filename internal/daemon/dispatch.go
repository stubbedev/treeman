package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/stubbedev/treeman/internal/db/binlog"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/template"
	"github.com/stubbedev/treeman/internal/version"
	"github.com/stubbedev/treeman/internal/watcher"
)

// Dispatch executes one RPC request against the live state, returning
// the response to send back over the socket.
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
		safeGo("wt_finalize", func() {
			bg := st.BgCtx
			err := FinalizeWorktree(bg, st, args.RepoPath, args.WorktreePath, args.InheritedEnv)
			if err != nil {
				_ = st.Store.WriteEvent(bg, "error", "wt_finalize", err.Error(),
					0, 0, "", 0, map[string]string{
						"repo_path": args.RepoPath, "worktree_path": args.WorktreePath,
					})
			}
		})
		return rpc.Response{Kind: rpc.KindWorktreeFinalizeQueued, WorktreePath: args.WorktreePath}

	case rpc.MethodWorktreeTeardown:
		if req.WorktreeTeardown == nil {
			return errResp("worktree_teardown: missing args")
		}
		args := *req.WorktreeTeardown
		safeGo("wt_teardown", func() {
			bg := st.BgCtx
			err := TeardownWorktree(bg, st, args.RepoPath, args.WorktreePath, args.Force, args.InheritedEnv)
			if err != nil {
				_ = st.Store.WriteEvent(bg, "error", "wt_teardown", err.Error(),
					0, 0, "", 0, map[string]string{
						"repo_path": args.RepoPath, "worktree_path": args.WorktreePath,
					})
			}
		})
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

	case rpc.MethodConfigReload:
		repoPath := ""
		if req.ConfigReload != nil {
			repoPath = req.ConfigReload.RepoPath
		}
		if st.ConfigReloader == nil {
			return errResp("config_reload: reloader not initialised")
		}
		if repoPath == "" {
			st.ConfigReloader.ReloadAll(st.BgCtx)
		} else {
			st.ConfigReloader.ReloadRepo(st.BgCtx, repoPath)
		}
		return rpc.Response{Kind: rpc.KindOk}

	case rpc.MethodRepoRemove:
		if req.RepoRemove == nil || req.RepoRemove.RepoPath == "" {
			return errResp("repo_remove: missing repo_path")
		}
		if err := removeRepoFromRegistry(ctx, st, req.RepoRemove.RepoPath, req.RepoRemove.Force); err != nil {
			return errResp(err.Error())
		}
		return rpc.Response{Kind: rpc.KindOk}

	case rpc.MethodWorktreeList:
		if req.WorktreeList == nil {
			return errResp("worktree_list: missing args")
		}
		paths, err := listWorktreePaths(ctx, st, req.WorktreeList.RepoPath)
		if err != nil {
			return errResp(err.Error())
		}
		return rpc.Response{
			Kind:      rpc.KindWorktreeList,
			RepoPath:  req.WorktreeList.RepoPath,
			Worktrees: paths,
		}

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

// listWorktreePaths returns the active worktree paths for a repo,
// either filtered by repoPath (when supplied) or every active one.
// Reads directly from SQLite — the daemon's source of truth for the
// worktree registry.
func listWorktreePaths(ctx context.Context, st *State, repoPath string) ([]string, error) {
	var (
		rows interface {
			Close() error
			Next() bool
			Scan(...any) error
			Err() error
		}
		err error
	)
	if repoPath == "" {
		rows, err = st.Store.DB.QueryContext(ctx,
			`SELECT path FROM worktrees WHERE deleted_at IS NULL ORDER BY id`)
	} else {
		rows, err = st.Store.DB.QueryContext(ctx, `
			SELECT w.path FROM worktrees w
			JOIN repos r ON r.id = w.repo_id
			WHERE w.deleted_at IS NULL AND r.path = ?
			ORDER BY w.id`, repoPath)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResumeRepoWatcher is the public boot-time entrypoint; daemon main
// loops over `ListRepoPaths` and calls this. Delegates to the same
// path the WatcherStart RPC uses.
func ResumeRepoWatcher(ctx context.Context, st *State, repoPath string) error {
	return startRepoWatcher(ctx, st, repoPath)
}

// ResumeWorktreeWatcher (re)spawns the per-worktree fsnotify watcher
// for a live worktree on daemon boot. No-op when already registered.
func ResumeWorktreeWatcher(ctx context.Context, st *State, repoPath, wtPath string) error {
	return startWorktreeWatcher(ctx, st, repoPath, wtPath)
}

// startRepoWatcher boots one binlog.Replicator goroutine per MySQL
// source database declared in the repo's .treeman.yaml — if
// `watcher.binlog.enabled = true`. Filesystem-event watching now
// lives in `startWorktreeWatcher` (rooted in each worktree's own
// checkout) because migrations and dumps follow the worktree's
// branch, not the main repo's.
//
// Each replicator runs until the WatcherEntry's cancel is invoked
// or the daemon shuts down. State tracking lets `watcher_list` /
// `status` report the running count.
func startRepoWatcher(ctx context.Context, st *State, repoPath string) error {
	if repoPath == "" {
		return fmt.Errorf("watcher_start: empty repo_path")
	}
	cfg, err := resolve.LoadResolved(repoPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}
	// Subscribe the config reloader to this repo's YAML files so a
	// live edit will trigger a watcher restart for this repo.
	st.ConfigReloader.AddRepo(repoPath)

	wctx, cancel := context.WithCancel(st.BgCtx)
	entry := &WatcherEntry{
		RepoPath:      repoPath,
		WorktreeCount: 0,
		Cancel:        cancel,
	}
	st.RegisterWatcher(repoPath, entry)

	binlogReps := 0
	if cfg.Watcher.Binlog.Enabled {
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
			r, src := r, sourceDB
			safeGo("binlog_replicator:"+repoPath+":"+src, func() {
				defer r.Stop()
				if err := r.Start(wctx); err != nil {
					slog.Warn("binlog replicator exit", "repo", repoPath, "source_db", src, "err", err)
				}
			})
			binlogReps++
		}
	}

	// Lifecycle watcher: gated on both per-repo opt-in
	// (`repos.watch_lifecycle = 1`) and resolved config
	// (`worktrees.hook_lifecycle: true`).
	if optIn, _ := st.Store.GetRepoWatchLifecycle(ctx, repoID); optIn && LifecycleEnabledForRepo(repoPath) {
		if !st.HasLifecycleWatcher(repoPath) {
			if _, err := StartLifecycleWatcher(ctx, st, repoID, repoPath); err != nil {
				slog.Warn("lifecycle watcher start failed", "repo", repoPath, "err", err)
			}
		}
	}

	slog.Info("repo watcher started",
		"repo", repoPath, "binlog_replicators", binlogReps)
	return nil
}

// startWorktreeWatcher spawns one fsnotify watcher rooted in the
// worktree's checkout. Edits to migration source files (or any path
// matched by `watcher.paths`) inside the worktree trigger a
// `FinalizeWorktree` rerun for just that worktree. Idempotent — a
// second call for the same wtPath is a no-op.
func startWorktreeWatcher(ctx context.Context, st *State, repoPath, wtPath string) error {
	if wtPath == "" {
		return fmt.Errorf("watcher_start: empty worktree_path")
	}
	if st.HasWtWatcher(wtPath) {
		return nil
	}
	// A teardown in flight will rm -rf this checkout shortly; starting
	// a watcher now just feeds the dispatcher REMOVE events that
	// re-spawn FinalizeWorktree against a dying tree.
	if st.IsTeardownInFlight(wtPath) {
		return nil
	}
	cfg, err := resolve.LoadResolvedForWorktree(repoPath, wtPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if len(cfg.Watcher.Paths) == 0 {
		return nil
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}

	wctx, cancel := context.WithCancel(st.BgCtx)
	dispatch := makeWtFSDispatcher(st, repoPath, repoID, wtPath)
	w, err := watcher.New(wtPath, cfg.Watcher, dispatch)
	if err != nil {
		cancel()
		return fmt.Errorf("fsnotify watcher init: %w", err)
	}
	// Close fsw directly on cancel — the watcher's Start loop selects
	// against both ctx.Done() and the fsnotify event channel. With a
	// backlog of REMOVE events (`git worktree remove --force` rm -rf'ing
	// vendor/, node_modules/) the Go runtime can keep picking events
	// many times before ctx.Done() wins the select. Closing fsw drains
	// the channel and forces the loop out immediately. Mirrors
	// LifecycleWatcher's cancel.
	entry := &WatcherEntry{
		RepoPath: repoPath,
		Cancel: func() {
			cancel()
			w.Stop()
		},
	}
	st.RegisterWtWatcher(wtPath, entry)
	safeGo("wt_fs_watcher:"+wtPath, func() {
		if err := w.Start(wctx); err != nil {
			slog.Warn("fsnotify watcher exit", "wt", wtPath, "err", err)
		}
	})
	slog.Info("worktree watcher started", "repo", repoPath, "wt", wtPath)
	return nil
}

// makeWtFSDispatcher builds a watcher.Dispatcher bound to a single
// worktree. Each event materialises as a `FinalizeWorktree` rerun
// for that worktree.
func makeWtFSDispatcher(st *State, repoPath string, repoID int64, wtPath string) watcher.Dispatcher {
	return func(ctx context.Context, ev watcher.Event) error {
		// Drop events while a teardown is in flight — finalising a
		// worktree the user just asked to delete is a feedback loop
		// (DB writes against a dying tree, watcher re-registration,
		// etc.).
		if st.IsTeardownInFlight(wtPath) {
			return nil
		}
		_ = st.Store.WriteEvent(ctx, "info", "watcher_fired",
			fmt.Sprintf("%s (%s)", ev.Path, ev.Mode),
			repoID, 0, "", 0, map[string]string{
				"path": ev.Path, "mode": string(ev.Mode), "wt": wtPath,
			})
		safeGo("watcher_finalize:"+wtPath, func() {
			if err := FinalizeWorktree(st.BgCtx, st, repoPath, wtPath, nil); err != nil {
				slog.Warn("watcher-triggered finalize", "wt", wtPath, "err", err)
			}
		})
		return nil
	}
}
