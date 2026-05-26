package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/runid"
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
			bg := runid.With(st.BgCtx, runid.New())
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
			bg := runid.With(st.BgCtx, runid.New())
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

	case rpc.MethodSyncNow:
		target := ""
		if req.SyncNow != nil {
			target = req.SyncNow.Path
		}
		statuses, errs := SyncNow(ctx, st, target)
		return rpc.Response{
			Kind:        rpc.KindSyncResult,
			SyncedRepos: statuses,
			SyncErrors:  errs,
		}

	case rpc.MethodSyncStatus:
		filter := ""
		if req.SyncStatus != nil {
			filter = req.SyncStatus.RepoPath
		}
		statuses := SyncStatusSnapshot(ctx, st, filter)
		return rpc.Response{
			Kind:        rpc.KindSyncStatus,
			SyncedRepos: statuses,
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

// startRepoWatcher attaches the per-repo background services that
// don't live per-worktree: the config reloader subscription and the
// lifecycle watcher (which observes `git worktree add/remove` events
// fired outside the treeman CLI). Filesystem-event watching lives
// per-worktree in `startWorktreeWatcher` because migrations and
// dumps follow each worktree's branch checkout.
//
// Runs until the WatcherEntry's cancel is invoked or the daemon
// shuts down. State tracking lets `watcher_list` / `status` report
// the running set.
func startRepoWatcher(ctx context.Context, st *State, repoPath string) error {
	if repoPath == "" {
		return fmt.Errorf("watcher_start: empty repo_path")
	}
	if _, err := resolve.LoadResolved(repoPath); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}
	// Subscribe the config reloader to this repo's YAML files so a
	// live edit will trigger a watcher restart for this repo.
	st.ConfigReloader.AddRepo(repoPath)

	_, cancel := context.WithCancel(st.BgCtx)
	entry := &WatcherEntry{
		RepoPath:      repoPath,
		WorktreeCount: 0,
		Cancel:        cancel,
	}
	st.RegisterWatcher(repoPath, entry)

	// Lifecycle watcher: always on. Worktree add/remove events outside
	// of the treeman CLI need the daemon to react regardless of any
	// opt-in toggle.
	if !st.HasLifecycleWatcher(repoPath) {
		if _, err := StartLifecycleWatcher(ctx, st, repoID, repoPath); err != nil {
			slog.Warn("lifecycle watcher start failed", "repo", repoPath, "err", err)
		}
	}

	slog.Info("repo watcher started", "repo", repoPath)
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

	// HEAD watcher — always-on. 100ms debounce is plenty: git
	// rewrites HEAD atomically (tmp file + rename = one fsnotify
	// event), so we only need to coalesce against the rare ref-pack
	// race. A tight window means `git checkout` → prefetch starts
	// within ~100ms instead of ~500ms perceived latency.
	hw, err := NewHeadWatcher(wtPath, 100*time.Millisecond, func(_ context.Context, newRef string) {
		rid := runid.New()
		evCtx := runid.With(st.BgCtx, rid)
		_ = st.Store.WriteEvent(evCtx, "info", "head_changed",
			fmt.Sprintf("HEAD → %s", newRef),
			repoID, 0, "", 0, map[string]string{
				"wt":  wtPath,
				"ref": newRef,
			})
		// Rehydrate the user's env captured at the last `wt create` /
		// `wt finalize` so PATH-sensitive scripts work in this
		// daemon-driven re-run.
		env, _ := st.Store.LoadInheritedEnvByPath(st.BgCtx, wtPath)
		// Fire user-defined on-checkout actions in parallel with
		// the regular finalize re-run (no ordering — both react to
		// the same event independently).
		safeGo("head_actions:"+wtPath, func() {
			fireTriggerActions(runid.With(st.BgCtx, rid), st, repoPath, wtPath, "on-checkout", env,
				func(cfg *config.Config) []config.Action { return cfg.Hooks.OnCheckout })
		})
		safeGo("head_finalize:"+wtPath, func() {
			if err := FinalizeWorktree(runid.With(st.BgCtx, rid), st, repoPath, wtPath, env); err != nil {
				slog.Warn("head-triggered finalize", "wt", wtPath, "err", err)
			}
		})
		// Sync the new branch against upstream — switching to a
		// stale branch otherwise leaves it stale until the next
		// auto-fetch tick (up to 15 min). Respects per-repo opt-out
		// the same way the periodic sweep does.
		safeGo("head_sync:"+wtPath, func() {
			cfg, err := resolve.LoadResolved(repoPath)
			if err != nil || !cfg.AutoFetch.IsEnabled() {
				return
			}
			_ = SyncWorktree(runid.With(st.BgCtx, rid), st, repoID, wtPath, cfg.AutoFetch.ResolvedMode())
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

	// FS watcher — aggregate every per-DB `databases[i].watch[]`
	// entry (tagged with i = DBIndex) into a single list the fsnotify
	// driver subscribes to. Only spawn when at least one glob is
	// declared across the whole config.
	aggregatedPaths := aggregateWatches(&cfg)
	if len(aggregatedPaths) > 0 {
		dispatch := makeWtFSDispatcher(st, repoPath, repoID, wtPath)
		w, err := watcher.New(wtPath, aggregatedPaths, cfg.DebounceMs, dispatch)
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

// aggregateWatches walks every `databases[i].inputs[]` block and
// returns the flat list of paths the fsnotify driver subscribes
// to. Each entry is tagged with its DBIndex so the dispatcher can
// route the event back to its owning database. The optional
// `label:` on each input passes through for filtered hook dispatch.
func aggregateWatches(cfg *config.Config) []config.WatcherPath {
	total := 0
	for _, db := range cfg.Databases {
		total += len(db.Inputs)
	}
	out := make([]config.WatcherPath, 0, total)
	for i, db := range cfg.Databases {
		for _, in := range db.Inputs {
			out = append(out, config.WatcherPath{
				Glob:    in.Glob,
				Label:   in.Label,
				DBIndex: i,
			})
		}
	}
	return out
}

// makeWtFSDispatcher builds a watcher.Dispatcher bound to a single
// worktree. Each event re-runs finalize, scoped to one database via
// the matched input's DBIndex. The cache-hit / cold-build decision
// is derived purely from the input fingerprint — no force-rebuild
// override.
func makeWtFSDispatcher(st *State, repoPath string, repoID int64, wtPath string) watcher.Dispatcher {
	return func(ctx context.Context, ev watcher.Event) error {
		// Drop events while a teardown is in flight — finalising a
		// worktree the user just asked to delete is a feedback loop
		// (DB writes against a dying tree, watcher re-registration,
		// etc.).
		if st.IsTeardownInFlight(wtPath) {
			return nil
		}
		// One run_id covers the watcher_fired event + the actions and
		// finalize fan-out that descend from it. The two safeGos use
		// st.BgCtx (so they outlive the dispatcher), so we explicitly
		// re-attach the id there too.
		rid := runid.New()
		evCtx := runid.With(ctx, rid)
		_ = st.Store.WriteEvent(evCtx, "info", "watcher_fired",
			fmt.Sprintf("%s (db_idx=%d label=%s)", ev.Path, ev.DBIndex, ev.Label),
			repoID, 0, "", 0, map[string]string{
				"path":   ev.Path,
				"db_idx": fmt.Sprintf("%d", ev.DBIndex),
				"label":  ev.Label,
				"wt":     wtPath,
			})
		dbIdx, path, label := ev.DBIndex, ev.Path, ev.Label
		env, _ := st.Store.LoadInheritedEnvByPath(st.BgCtx, wtPath)
		// Fire the on-file-change actions in parallel with the
		// re-prep — both react to the same event independently.
		safeGo("watch_actions:"+wtPath, func() {
			fireOnFileChange(runid.With(st.BgCtx, rid), st, repoPath, wtPath, dbIdx, path, label, env)
		})
		safeGo("watcher_finalize:"+wtPath, func() {
			if err := FinalizeWorktreeForWatch(runid.With(st.BgCtx, rid), st, repoPath, wtPath, dbIdx, env); err != nil {
				slog.Warn("watcher-triggered finalize", "wt", wtPath, "err", err)
			}
		})
		return nil
	}
}

// fireOnFileChange runs the global `hooks.on-file-change` actions,
// filtered by the watch event's label. Actions with an empty
// `match:` fire for any watch event (any engine, any label);
// actions with `match: <label>` only fire when the event's label
// matches.
//
// The subprocess receives watch context as env vars so user scripts
// can branch on the trigger details: TREEMAN_WATCH_PATH,
// TREEMAN_WATCH_LABEL, TREEMAN_WATCH_ENGINE, TREEMAN_WATCH_DB_NAME.
//
// Main-wt aware: resolveWorktreeIdentity must run BEFORE we read the
// owning database's NameTemplate, otherwise the overlay's
// main-wt-specific template wouldn't be in cfg.Databases yet and
// TREEMAN_WATCH_DB_NAME would resolve to the linked-wt template's
// rendering. Same call also forces slug.ForMain + EnsureMainWorktree
// for the repo-root path so $TREEMAN_SLUG matches the finalize path
// and the main row's slug column isn't overwritten with a path hash.
func fireOnFileChange(
	ctx context.Context,
	st *State,
	repoPath, wtPath string,
	dbIdx int,
	eventPath, label string,
	inheritedEnv map[string]string,
) {
	cfg, err := resolve.LoadResolvedForWorktree(repoPath, wtPath)
	if err != nil {
		slog.Warn("on-file-change: load config", "wt", wtPath, "err", err)
		return
	}
	all := cfg.Hooks.OnFileChange
	if len(all) == 0 {
		return
	}
	// Filter by label match (empty Match list = wildcard).
	matched := make([]config.Action, 0, len(all))
	for _, fa := range all {
		if fa.Matches(label) {
			matched = append(matched, fa.Action)
		}
	}
	if len(matched) == 0 {
		return
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return
	}
	sl, wtID, _, _, err := resolveWorktreeIdentity(ctx, st, &cfg, repoPath, wtPath, repoID)
	if err != nil {
		return
	}
	// Resolve the owning database's engine + rendered name so the
	// hook can branch on engine without re-parsing config. cfg has
	// already been overlay-merged by resolveWorktreeIdentity, so
	// d.NameTemplate is the main-wt-specific template when relevant.
	var engine, dbName string
	if dbIdx >= 0 && dbIdx < len(cfg.Databases) {
		d := cfg.Databases[dbIdx]
		engine = d.Engine
		if rendered, err := template.Render(d.NameTemplate, template.FromSlug(sl)); err == nil {
			dbName = rendered
		}
	}
	// Layer the watch-context env on top of the user's cached env.
	env := make(map[string]string, len(inheritedEnv)+4)
	for k, v := range inheritedEnv {
		env[k] = v
	}
	env["TREEMAN_WATCH_PATH"] = eventPath
	env["TREEMAN_WATCH_LABEL"] = label
	env["TREEMAN_WATCH_ENGINE"] = engine
	env["TREEMAN_WATCH_DB_NAME"] = dbName
	_ = runTriggerActions(ctx, st, "on-file-change", matched, repoPath, wtPath, sl.Value, repoID, wtID, env)
}

// fireTriggerActions loads the resolved config for a worktree and
// runs the selected hooks trigger's actions. Used by the on-head-
// change and on-watch dispatch paths so user-defined hooks can
// react to those events without sharing the regular finalize logic.
// Errors are swallowed + logged; this path is best-effort.
//
// Main-wt aware via resolveWorktreeIdentity: when wtPath is the repo
// root and main-wt is enabled, the hook subprocess receives
// $TREEMAN_SLUG=main_<branch> matching the finalize-path slug. A
// stale slug.For here would corrupt the main row's slug column on
// every HEAD switch (EnsureWorktree overwrites slug keyed by path).
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
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return
	}
	sl, wtID, _, _, err := resolveWorktreeIdentity(ctx, st, &cfg, repoPath, wtPath, repoID)
	if err != nil {
		return
	}
	_ = runTriggerActions(ctx, st, trigger, actions, repoPath, wtPath, sl.Value, repoID, wtID, inheritedEnv)
}
