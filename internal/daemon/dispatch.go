package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/binlog"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/slug"
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

	// Lifecycle watcher: always on. Worktree add/remove events outside
	// of the treeman CLI need the daemon to react regardless of any
	// opt-in toggle.
	if !st.HasLifecycleWatcher(repoPath) {
		if _, err := StartLifecycleWatcher(ctx, st, repoID, repoPath); err != nil {
			slog.Warn("lifecycle watcher start failed", "repo", repoPath, "err", err)
		}
	}

	slog.Info("repo watcher started",
		"repo", repoPath, "binlog_replicators", binlogReps)
	return nil
}

// startWorktreeWatcher spawns the per-worktree watchers:
//
//   - HEAD watcher (always-on): tails .git/HEAD and re-runs finalize
//     when the user switches branches inside an existing worktree, so
//     patches re-evaluate against the new branch's slug and prepare
//     picks up any new migrations / dump.
//   - FS watcher (when `watcher.paths` is non-empty): watches the
//     configured globs and re-runs finalize on edits.
//
// Idempotent — a second call for the same wtPath is a no-op.
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
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}

	wctx, cancel := context.WithCancel(st.BgCtx)
	// Compound stop: closes both watchers when the entry is unregistered.
	stoppers := []func(){cancel}

	// HEAD watcher — always-on.
	hw, err := NewHeadWatcher(wtPath, 500*time.Millisecond, func(_ context.Context, newRef string) {
		_ = st.Store.WriteEvent(st.BgCtx, "info", "head_changed",
			fmt.Sprintf("HEAD → %s", newRef),
			repoID, 0, "", 0, map[string]string{
				"wt":  wtPath,
				"ref": newRef,
			})
		// Rehydrate the user's env captured at the last `wt create` /
		// `wt finalize` so PATH-sensitive scripts work in this
		// daemon-driven re-run.
		env, _ := st.Store.LoadInheritedEnvByPath(st.BgCtx, wtPath)
		// Fire user-defined on-head-change actions in parallel with
		// the regular finalize re-run (no ordering — both react to
		// the same event independently).
		safeGo("head_actions:"+wtPath, func() {
			fireTriggerActions(st.BgCtx, st, repoPath, wtPath, "on-head-change", env,
				func(cfg *config.Config) []config.Action { return cfg.Hooks.OnHeadChange })
		})
		safeGo("head_finalize:"+wtPath, func() {
			if err := FinalizeWorktree(st.BgCtx, st, repoPath, wtPath, env); err != nil {
				slog.Warn("head-triggered finalize", "wt", wtPath, "err", err)
			}
		})
	})
	if err != nil {
		slog.Warn("head watcher init failed", "wt", wtPath, "err", err)
	} else {
		stoppers = append(stoppers, hw.Stop)
		safeGo("wt_head_watcher:"+wtPath, func() {
			if err := hw.Start(wctx); err != nil {
				slog.Warn("head watcher exit", "wt", wtPath, "err", err)
			}
		})
	}

	// FS watcher — aggregate top-level `watcher.paths` (DBIndex = -1,
	// applies to every DB) and per-DB `databases[i].watch` entries
	// (DBIndex = i) into a single WatcherConfig consumed by the
	// fsnotify driver. Only spawn when at least one glob is declared.
	aggregated := aggregateWatches(&cfg)
	if len(aggregated.Paths) > 0 {
		dispatch := makeWtFSDispatcher(st, repoPath, repoID, wtPath)
		w, err := watcher.New(wtPath, aggregated, dispatch)
		if err != nil {
			cancel()
			return fmt.Errorf("fsnotify watcher init: %w", err)
		}
		stoppers = append(stoppers, w.Stop)
		safeGo("wt_fs_watcher:"+wtPath, func() {
			if err := w.Start(wctx); err != nil {
				slog.Warn("fsnotify watcher exit", "wt", wtPath, "err", err)
			}
		})
	}

	entry := &WatcherEntry{
		RepoPath: repoPath,
		Cancel: func() {
			// Cancel context first so any running dispatch sees ctx.Done()
			// before the underlying watchers tear down their channels.
			for _, fn := range stoppers {
				fn()
			}
		},
	}
	st.RegisterWtWatcher(wtPath, entry)
	slog.Info("worktree watcher started", "repo", repoPath, "wt", wtPath)
	return nil
}

// aggregateWatches builds the WatcherConfig the fsnotify driver
// actually subscribes to: every top-level `watcher.paths` entry
// (tagged DBIndex = -1, applies to all DBs) plus every per-DB
// `databases[i].watch` entry (tagged DBIndex = i). Top-level globs
// retain their position so a user-declared order is preserved.
func aggregateWatches(cfg *config.Config) config.WatcherConfig {
	out := cfg.Watcher
	out.Paths = make([]config.WatcherPath, 0, len(cfg.Watcher.Paths))
	for _, p := range cfg.Watcher.Paths {
		p.DBIndex = -1
		out.Paths = append(out.Paths, p)
	}
	for i, db := range cfg.Databases {
		for _, p := range db.Watch {
			p.DBIndex = i
			out.Paths = append(out.Paths, p)
		}
	}
	return out
}

// makeWtFSDispatcher builds a watcher.Dispatcher bound to a single
// worktree. Each event re-runs finalize, scoped to one database
// when the matched glob came from `databases[i].watch`, and forcing
// a full rebuild when the matched glob's `on:` is `rebuild`.
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
			fmt.Sprintf("%s (%s db_idx=%d)", ev.Path, ev.Mode, ev.DBIndex),
			repoID, 0, "", 0, map[string]string{
				"path":    ev.Path,
				"mode":    string(ev.Mode),
				"db_idx":  fmt.Sprintf("%d", ev.DBIndex),
				"wt":      wtPath,
			})
		mode, dbIdx := ev.Mode, ev.DBIndex
		env, _ := st.Store.LoadInheritedEnvByPath(st.BgCtx, wtPath)
		// Fire user-defined on-watch actions in parallel with the
		// regular re-prep — both react to the same event.
		safeGo("watch_actions:"+wtPath, func() {
			fireTriggerActions(st.BgCtx, st, repoPath, wtPath, "on-watch", env,
				func(cfg *config.Config) []config.Action { return cfg.Hooks.OnWatch })
		})
		safeGo("watcher_finalize:"+wtPath, func() {
			if err := FinalizeWorktreeForWatch(st.BgCtx, st, repoPath, wtPath, dbIdx, mode, env); err != nil {
				slog.Warn("watcher-triggered finalize", "wt", wtPath, "err", err)
			}
		})
		return nil
	}
}

// fireTriggerActions loads the resolved config for a worktree and
// runs the selected hooks trigger's actions. Used by the on-head-
// change and on-watch dispatch paths so user-defined hooks can
// react to those events without sharing the regular finalize logic.
// Errors are swallowed + logged; this path is best-effort.
func fireTriggerActions(
	ctx context.Context,
	st *State,
	repoPath, wtPath, trigger string,
	inheritedEnv map[string]string,
	pick func(*config.Config) []config.Action,
) {
	cfg, err := resolve.LoadResolvedForWorktree(repoPath, wtPath)
	if err != nil {
		slog.Warn("trigger actions: load config", "trigger", trigger, "wt", wtPath, "err", err)
		return
	}
	actions := pick(&cfg)
	if len(actions) == 0 {
		return
	}
	branch := detectBranch(wtPath)
	sl := slug.For(wtPath, branch)
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, wtPath, sl.Value, branch)
	if err != nil {
		return
	}
	_ = runTriggerActions(ctx, st, trigger, actions, repoPath, wtPath, sl.Value, repoID, wtID, inheritedEnv)
}
