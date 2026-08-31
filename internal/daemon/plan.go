package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/wt"
	"github.com/stubbedev/treeman/internal/wtreg"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// handleRunPlan executes a submitted plan. The daemon is the sole
// mutator of repo/worktree state; the CLI only builds plans and submits
// them here. Wait=true runs the plan inline and returns per-task results
// (for --json / result-oriented callers); Wait=false queues it in a
// background goroutine and returns immediately, emitting a terminal
// plan:end / plan:error event under the plan's run-id so a foreground
// subscriber knows when it finished.
func handleRunPlan(ctx context.Context, st *State, req rpc.Request) rpc.Response {
	if req.RunPlan == nil {
		return errResp("run_plan: missing args")
	}
	args := *req.RunPlan
	id := args.RunID
	if id == "" {
		id = runid.New()
	}

	if args.Wait {
		results := ExecutePlan(runid.With(ctx, id), st, args.Groups)
		return rpc.Response{Kind: rpc.KindPlanResult, TaskResults: results}
	}

	safeGo(lblPlanRun, "", func() {
		bg := runid.With(st.BgCtx, id)
		_ = st.Store.WriteEvent(bg, store.LevelInfo, store.EvtPlanStart, "plan beginning", 0, 0, "", 0, nil)
		results := ExecutePlan(bg, st, args.Groups)
		if msg := firstFailure(results); msg != "" {
			_ = st.Store.WriteEvent(bg, store.LevelError, store.EvtPlanError, msg, 0, 0, "", 0, nil)
			return
		}
		_ = st.Store.WriteEvent(bg, store.LevelInfo, store.EvtPlanEnd, "plan complete", 0, 0, "", 0, nil)
	})
	return rpc.Response{Kind: rpc.KindPlanQueued}
}

// RunPlanInProcess runs a plan locally — opening the store, building an
// ephemeral State, and executing — for callers (the CLI, the MCP server)
// that found no daemon reachable. The daemon is the *preferred* mutator,
// but treeman commands must still work daemon-less; this is that path,
// reusing the exact same task runners so behavior is identical. The
// create tail runs synchronously (SyncFinalize) since no daemon outlives
// the call. Always returns a KindPlanResult response.
func RunPlanInProcess(ctx context.Context, args rpc.RunPlanArgs) (rpc.Response, error) {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return rpc.Response{}, err
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return rpc.Response{}, err
	}
	defer func() { _ = st.Close() }()
	state := NewState(ctx, st)
	state.SyncFinalize = true
	return rpc.Response{Kind: rpc.KindPlanResult, TaskResults: ExecutePlan(ctx, state, args.Groups)}, nil
}

// ExecutePlan runs the plan: groups run in parallel; the tasks within a
// group run sequentially as one ordered lane, stopping that lane at its
// first failure (the remaining tasks in the lane depend on it). Results
// are returned lane-by-lane in submission order.
func ExecutePlan(ctx context.Context, st *State, groups [][]rpc.Task) []rpc.TaskResult {
	laneResults := make([][]rpc.TaskResult, len(groups))
	var wg sync.WaitGroup
	for i := range groups {
		wg.Add(1)
		lane := groups[i]
		safeGo(lblPlanLane, "", func() {
			defer wg.Done()
			for _, task := range lane {
				payload, err := runOneTask(ctx, st, task)
				res := rpc.TaskResult{Type: task.Type, OK: err == nil, PayloadJSON: string(payload)}
				if err != nil {
					res.Message = err.Error()
				}
				laneResults[i] = append(laneResults[i], res)
				if err != nil {
					break // stop this lane; later tasks depend on this one
				}
			}
		})
	}
	wg.Wait()
	var results []rpc.TaskResult
	for _, lane := range laneResults {
		results = append(results, lane...)
	}
	return results
}

// firstFailure returns the first failed task's message, or "" if all OK.
func firstFailure(results []rpc.TaskResult) string {
	for _, r := range results {
		if !r.OK {
			return fmt.Sprintf("%s: %s", r.Type, r.Message)
		}
	}
	return ""
}

// taskRunner mutates state via the daemon's warm store and returns a
// task-specific JSON payload (nil when there's nothing structured to
// return).
type taskRunner func(context.Context, *State, rpc.Task) (json.RawMessage, error)

// taskRunners is the full set of mutations the daemon performs — the one
// extension point of the plan model. Adding a task = add a Task* const
// (rpc), a runner, and one entry here.
var taskRunners = map[string]taskRunner{
	rpc.TaskPrepare:            runTaskPrepare,
	rpc.TaskDBReset:            runTaskDBReset,
	rpc.TaskDBSave:             runTaskDBSave,
	rpc.TaskHookRun:            runTaskHookRun,
	rpc.TaskSnapshotsPurge:     runTaskSnapshotsPurge,
	rpc.TaskSnapshotsPrune:     runTaskSnapshotsPrune,
	rpc.TaskMainPurgeDBs:       runTaskMainPurgeDBs,
	rpc.TaskWorktreeRegister:   runTaskWorktreeRegister,
	rpc.TaskWorktreeUnregister: runTaskWorktreeUnregister,
	rpc.TaskLogsPurge:          runTaskLogsPurge,
	rpc.TaskRegistryRepair:     runTaskRegistryRepair,
	rpc.TaskConfigWrite:        runTaskConfigWrite,
	rpc.TaskWorktreeCreate:     runTaskWorktreeCreate,
	rpc.TaskWorktreeFinalize:   runTaskWorktreeFinalize,
	rpc.TaskWorktreeTeardown:   runTaskWorktreeTeardown,
}

// runOneTask dispatches a single task to its registered runner.
//
// The caller's shell PATH (shipped as task.InheritedEnv["PATH"]) is
// pinned onto ctx so every git subprocess the runner spawns resolves
// programs the way the user's shell would — most importantly the
// clean/smudge filter git shells out as the bare `treeman patch-filter`
// during `git worktree add` checkout. Without this the daemon's stock
// systemd PATH can't find `treeman` and the filter fails (exit 127),
// which with filter.required aborts the whole worktree add.
func runOneTask(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	run, ok := taskRunners[task.Type]
	if !ok {
		return nil, fmt.Errorf("run_plan: unknown task %q", task.Type)
	}
	ctx = gitcmd.WithPath(ctx, task.InheritedEnv["PATH"])
	return run(ctx, st, task)
}

func runTaskWorktreeFinalize(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	return nil, FinalizeWorktree(ctx, st, task.RepoPath, task.WorktreePath, task.InheritedEnv,
		task.Params[rpc.ParamSkipPrepare] == "1")
}

func runTaskWorktreeTeardown(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	return nil, TeardownWorktree(ctx, st, task.RepoPath, task.WorktreePath, task.Params[rpc.ParamForce] == "1", task.InheritedEnv)
}

// taskIdentity bundles the resolved config + row identity a worktree-
// scoped task needs. Mirrors the FinalizeWorktree preamble.
type taskIdentity struct {
	cfg    config.Config
	sl     slug.Slug
	repoID int64
	wtID   int64
	isMain bool
}

func resolveTaskIdentity(ctx context.Context, st *State, repoPath, wtPath string) (taskIdentity, error) {
	cfg, err := resolve.LoadResolvedForWorktree(repoPath, wtPath)
	if err != nil {
		return taskIdentity{}, err
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return taskIdentity{}, err
	}
	id, err := wt.ResolveIdentity(ctx, st.Store, &cfg, repoPath, wtPath, detectBranch(wtPath), repoID)
	if err != nil {
		return taskIdentity{}, err
	}
	return taskIdentity{cfg: cfg, sl: id.Slug, repoID: repoID, wtID: id.WtID, isMain: id.IsMain}, nil
}

func runTaskPrepare(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	id, err := resolveTaskIdentity(ctx, st, task.RepoPath, task.WorktreePath)
	if err != nil {
		return nil, err
	}
	outs, err := lockedPrepare(ctx, st, id, task)
	if err != nil {
		return nil, err
	}
	// Same post-hook contract as finalize: prepare mutated engine
	// content, so hooks that react to fresh data (cache flushes) fire.
	if err := runTriggerActions(ctx, st, "create-after-engines",
		id.cfg.Hooks.OnCreateAfterEngines, task.RepoPath, task.WorktreePath,
		id.sl.Value, id.isMain, id.repoID, id.wtID, task.InheritedEnv); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"outcomes": outs})
}

// lockedPrepare runs prepare.Run for a task under the worktree's
// engine-prepare lock, so a queued prepare can't collide with the
// finalize pipeline, a watcher re-prepare, or another prepare task
// building the same source databases (issue #28).
func lockedPrepare(ctx context.Context, st *State, id taskIdentity, task rpc.Task) ([]prepare.Outcome, error) {
	prepLk := st.LockWorktreePrepare(task.WorktreePath)
	prepLk.Lock()
	defer prepLk.Unlock()
	return prepare.Run(ctx, &id.cfg, task.WorktreePath, id.sl, st.Store, id.repoID, id.wtID, task.InheritedEnv)
}

func runTaskDBReset(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	id, err := resolveTaskIdentity(ctx, st, task.RepoPath, task.WorktreePath)
	if err != nil {
		return nil, err
	}
	engineFilter := task.Params[rpc.ParamEngineFilter]
	// Reset + rebuild are one unit: dropping the branch-scoped state and
	// then racing a concurrent build against the same databases is the
	// collision issue #28 describes, so both run under the lock.
	prepLk := st.LockWorktreePrepare(task.WorktreePath)
	prepLk.Lock()
	resetErr := prepare.ResetBranchScoped(ctx, &id.cfg, task.WorktreePath, id.repoID, id.wtID, st.Store, engineFilter)
	var outs []prepare.Outcome
	if resetErr == nil {
		outs, resetErr = prepare.Run(ctx, &id.cfg, task.WorktreePath, id.sl, st.Store, id.repoID, id.wtID, task.InheritedEnv)
	}
	prepLk.Unlock()
	if resetErr != nil {
		return nil, resetErr
	}
	// db_reset re-seeded content — post-hooks fire like any prepare.
	if err := runTriggerActions(ctx, st, "create-after-engines",
		id.cfg.Hooks.OnCreateAfterEngines, task.RepoPath, task.WorktreePath,
		id.sl.Value, id.isMain, id.repoID, id.wtID, task.InheritedEnv); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"outcomes": seededOutcomes(outs, &id.cfg, engineFilter)})
}

func runTaskDBSave(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	id, err := resolveTaskIdentity(ctx, st, task.RepoPath, task.WorktreePath)
	if err != nil {
		return nil, err
	}
	saves, err := prepare.SaveBranchScoped(
		ctx,
		&id.cfg,
		task.WorktreePath,
		id.repoID,
		id.wtID,
		st.Store,
		task.Params[rpc.ParamEngineFilter],
	)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"saved": saves})
}

// seededOutcomes filters prepare outcomes to the branch-scoped databases
// the reset actually touched, honoring an optional engine filter. Mirrors
// the CLI's RunDbResetOnWorktree post-filter.
func seededOutcomes(outs []prepare.Outcome, cfg *config.Config, engineFilter string) []prepare.Outcome {
	filterLabel := engineFilter
	if engineFilter != "" {
		if fam, ok := engine.Canonical(engineFilter); ok {
			filterLabel = string(fam)
		}
	}
	var seeded []prepare.Outcome
	for _, o := range outs {
		for _, d := range cfg.Databases {
			if !d.BranchScoped {
				continue
			}
			fam, ok := engine.Canonical(d.Engine)
			if !ok {
				continue
			}
			label := string(fam)
			if filterLabel != "" && label != filterLabel {
				continue
			}
			if label == o.Engine {
				seeded = append(seeded, o)
				break
			}
		}
	}
	return seeded
}

func runTaskHookRun(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	id, err := resolveTaskIdentity(ctx, st, task.RepoPath, task.WorktreePath)
	if err != nil {
		return nil, err
	}
	phase := task.Params[rpc.ParamPhase]
	var entries []config.Action
	switch phase {
	case "create-before-engines":
		entries = id.cfg.Hooks.OnCreateBeforeEngines
	case "create-after-engines":
		entries = id.cfg.Hooks.OnCreateAfterEngines
	case "delete-before-engines":
		entries = id.cfg.Hooks.OnDeleteBeforeEngines
	case "delete-after-engines":
		entries = id.cfg.Hooks.OnDeleteAfterEngines
	case "checkout":
		entries = id.cfg.Hooks.OnCheckout
	default:
		return nil, fmt.Errorf("run_plan hook_run: unknown phase %q", phase)
	}
	started := hooks.EmitHookStart(ctx, st.Store, id.repoID, id.wtID, phase, len(entries))
	out, runErr := hooks.RunHooks(ctx, phase, entries, task.RepoPath,
		task.WorktreePath, id.sl.Value, id.isMain, task.InheritedEnv, true)
	hooks.PersistOutcome(ctx, st.Store, id.repoID, id.wtID, phase, started, time.Now().UnixMilli(), out)
	if runErr != nil {
		return nil, runErr
	}
	return json.Marshal(map[string]any{"phase": phase, "outcome": out})
}

// resolveRepoTask is the preamble for repo-scoped tasks (no worktree):
// load the repo's resolved config + ensure its row. Mirror of
// resolveTaskIdentity for the worktree-scoped tasks.
func resolveRepoTask(ctx context.Context, st *State, repoPath string) (config.Config, int64, error) {
	cfg, err := resolve.LoadResolved(repoPath)
	if err != nil {
		return config.Config{}, 0, err
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	return cfg, repoID, err
}

func runTaskSnapshotsPurge(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	cfg, repoID, err := resolveRepoTask(ctx, st, task.RepoPath)
	if err != nil {
		return nil, err
	}
	dropped, errs := snapshot.PurgeRepo(ctx, &cfg, st.Store, repoID)
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsPurgeEnd,
		fmt.Sprintf("dropped %d snapshot(s)", dropped), repoID, 0, "", 0, nil)
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		slog.Warn("snapshots purge", "repo", task.RepoPath, "err", e)
		msgs = append(msgs, e.Error())
	}
	payload, mErr := json.Marshal(map[string]any{"repo": task.RepoPath, "dropped": dropped, "errors": msgs})
	if mErr != nil {
		return nil, mErr
	}
	if len(errs) > 0 {
		return payload, errs[0]
	}
	return payload, nil
}

func runTaskSnapshotsPrune(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	cfg, repoID, err := resolveRepoTask(ctx, st, task.RepoPath)
	if err != nil {
		return nil, err
	}
	pruned, errs := snapshot.PruneRowOrphans(ctx, &cfg, st.Store, repoID)
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		slog.Warn("snapshots prune", "repo", task.RepoPath, "err", e)
		msgs = append(msgs, e.Error())
	}
	payload, mErr := json.Marshal(map[string]any{"repo": task.RepoPath, "pruned": pruned, "count": len(pruned), "errors": msgs})
	if mErr != nil {
		return nil, mErr
	}
	if len(errs) > 0 {
		return payload, errs[0]
	}
	return payload, nil
}

func runTaskMainPurgeDBs(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	cfg, repoID, err := resolveRepoTask(ctx, st, task.RepoPath)
	if err != nil {
		return nil, err
	}
	// Apply the main-wt overlay so TeardownDatabases probes the same
	// names the main-wt finalize path created (mirrors purgeMainDatabases).
	config.ApplyMainWorktreeOverlay(&cfg)

	branches := localBranchesOf(ctx, task.RepoPath)
	if current := detectBranch(task.RepoPath); current != "" && !slices.Contains(branches, current) {
		branches = append(branches, current)
	}
	var purged int
	for _, branch := range branches {
		sl := slug.ForMain(task.RepoPath, branch)
		if err := prepare.TeardownDatabases(ctx, &cfg, sl.Value, repoID, 0, st.Store); err != nil {
			slog.Warn("main purge", "slug", sl.Value, "err", err)
			continue
		}
		purged++
	}
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtMainPurge,
		fmt.Sprintf("tore down DBs for %d branch(es)", purged), repoID, 0, "", 0, nil)
	return json.Marshal(map[string]any{"purged": purged})
}

func runTaskWorktreeRegister(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	repoID, err := st.Store.EnsureRepo(ctx, task.RepoPath, filepath.Base(task.RepoPath))
	if err != nil {
		return nil, err
	}
	branch := task.Params[rpc.ParamBranch]
	sl := slug.For(task.WorktreePath, branch)
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, task.WorktreePath, sl.Value, branch)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"worktree_id": wtID, "slug": sl.Value, "repo_id": repoID})
}

func runTaskWorktreeUnregister(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	row := st.Store.DB.QueryRowContext(ctx,
		"SELECT id FROM worktrees WHERE path = ? COLLATE NOCASE", task.WorktreePath)
	var id int64
	if err := row.Scan(&id); err != nil {
		return nil, fmt.Errorf("worktree not found: %s", task.WorktreePath)
	}
	if err := st.Store.MarkWorktreeDeleted(ctx, id); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"worktree_id": id})
}

func runTaskLogsPurge(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	f := store.EventFilter{}
	if v := task.Params[rpc.ParamLevels]; v != "" {
		f.Levels = strings.Split(v, ",")
	}
	if v := task.Params[rpc.ParamEventTypes]; v != "" {
		f.EventTypes = strings.Split(v, ",")
	}
	if v := task.Params[rpc.ParamUntilMs]; v != "" {
		f.UntilMs, _ = strconv.ParseInt(v, 10, 64)
	}
	// Scope to a repo / worktree by resolving the (CLI-absolutized) paths
	// to row ids — mirrors the CLI's applyPurgeScope.
	if repo := task.Params[rpc.ParamRepo]; repo != "" {
		if rid, err := st.Store.LookupRepoID(ctx, repo); err == nil {
			f.RepoID = rid
		}
	}
	if wtArg := task.Params[rpc.ParamWorktree]; wtArg != "" {
		wid, err := st.Store.LookupWorktreeID(ctx, f.RepoID, wtArg)
		if err != nil {
			return nil, err
		}
		if wid == 0 {
			return nil, fmt.Errorf("no worktree matches %q", wtArg)
		}
		f.WorktreeID = wid
	}
	n, err := st.Store.PurgeEvents(ctx, f)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"rows_removed": n})
}

func runTaskRegistryRepair(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	res, err := wtreg.Repair(ctx, st.Store, task.RepoPath, detectBranch)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

// runTaskConfigWrite persists a CLI-rendered config body: snapshot the
// previous content into config_history, atomic-write the new body, then
// reload the repo. The daemon is the sole writer of .treeman.yaml — this
// keeps the write coherent with the daemon's own config watcher (the
// watcher's later REMOVE/CREATE fire is a harmless idempotent re-reload).
func runTaskConfigWrite(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	path := task.Params[rpc.ParamPath]
	if path == "" {
		return nil, errors.New("config_write: missing path")
	}
	body := []byte(task.Params[rpc.ParamBody])
	if prev, err := os.ReadFile(path); err == nil {
		_, _ = st.Store.SnapshotConfig(ctx, task.RepoPath, path, prev)
	}
	if err := yamlpatch.AtomicWrite(path, body); err != nil {
		return nil, err
	}
	if st.ConfigReloader != nil && task.RepoPath != "" {
		st.ConfigReloader.ReloadRepo(ctx, task.RepoPath)
	}
	return json.Marshal(map[string]any{"bytes": len(body)})
}

// runTaskWorktreeCreate performs the git-front of worktree creation
// (git worktree add + register + port alloc) on the daemon's warm store,
// then kicks the hooks+prepare finalize tail asynchronously (under
// BgCtx, so it outlives the result RPC) and returns the new path + ids.
func runTaskWorktreeCreate(ctx context.Context, st *State, task rpc.Task) (json.RawMessage, error) {
	req := wt.CreateRequest{
		RepoRoot:    task.RepoPath,
		Branch:      task.Params[rpc.ParamBranch],
		From:        task.Params[rpc.ParamFrom],
		Path:        task.Params[rpc.ParamPath],
		NoFetch:     task.Params[rpc.ParamNoFetch] == "1",
		SkipHooks:   task.Params[rpc.ParamSkipHooks] == "1",
		SkipPrepare: task.Params[rpc.ParamSkipPrepare] == "1",
		Env:         task.InheritedEnv,
	}
	res, needsFinalize, err := wt.CreateInStore(ctx, req, st.Store, nil)
	if err != nil {
		return nil, err
	}
	if needsFinalize {
		repoRoot, wtPath, env := req.RepoRoot, res.WtPath, req.Env
		runFinalize := func(bg context.Context) {
			if ferr := FinalizeWorktree(bg, st, repoRoot, wtPath, env, req.SkipPrepare); ferr != nil {
				_ = st.Store.WriteEvent(bg, store.LevelError, store.EvtWorktreeCreateError, ferr.Error(),
					0, 0, "", 0, map[string]string{"repo_path": repoRoot, "worktree_path": wtPath})
			}
		}
		if st.SyncFinalize {
			// No daemon to outlive the call — run the tail before returning.
			runFinalize(runid.With(ctx, runid.New()))
		} else {
			safeGo(lblWorktreeFinalize, "", func() { runFinalize(runid.With(st.BgCtx, runid.New())) })
		}
		res.Status = wt.CreatedQueued
	}
	return json.Marshal(res)
}

// localBranchesOf lists the repo's local branches. Empty on error —
// purge is best-effort. Mirrors the CLI's localBranches.
func localBranchesOf(ctx context.Context, repoRoot string) []string {
	out, err := gitcmd.String(ctx, repoRoot,
		"for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil
	}
	var branches []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}
