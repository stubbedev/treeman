package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/patcher"
	"github.com/stubbedev/treeman/internal/ports"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
	"github.com/stubbedev/treeman/internal/wt"
)

func nowMillis() int64 { return time.Now().UnixMilli() }

// FinalizeWorktree is the daemon's tokio-equivalent (just a Go
// goroutine) tail of `treeman wt create`: it runs the create
// hook pipeline + engine prepare against the main repo root,
// detached from the CLI invocation.
func FinalizeWorktree(
	ctx context.Context,
	st *State,
	repoPath, worktreePath string,
	inheritedEnv map[string]string,
) (err error) {
	repoRoot := repoPath
	wtRoot := worktreePath
	// Captured by the terminal-event defer below. Populated once the
	// row identity is known so a hook/prepare error after that point
	// always produces a worktree:create:error event scoped to the right row.
	var (
		termRepoID int64
		termWtID   int64
	)
	defer func() {
		if err == nil {
			return
		}
		if termWtID == 0 {
			// Pre-identity error (LoadResolved, EnsureRepo,
			// ResolveIdentity): we don't have a row to attach a
			// terminal event to. Let the dispatch-level safety net at
			// dispatch.go log a global worktree:create:error error instead.
			return
		}
		// Terminal event for `finalizeState` — without this, a
		// prepare/hook failure leaves the row derived as preparing
		// forever (see #11). Swallow the returned error afterwards so
		// dispatch doesn't double-log the same failure as a second
		// (row-less) worktree:create:error event.
		_ = st.Store.WriteEvent(ctx, store.LevelError, store.EvtWorktreeCreateError, err.Error(),
			termRepoID, termWtID, "", 0, map[string]string{
				"repo_path":     repoRoot,
				"worktree_path": wtRoot,
			})
		err = nil
	}()

	// Derive a cancellable, DEADLINE-bound ctx. Two jobs:
	//   1. TeardownWorktree preempts via CancelFinalize (the cancel func is
	//      registered with MarkFinalizeInFlight below). ctx.Err() is consulted
	//      at each phase boundary so a concurrent `wt delete` preempts before
	//      prepare.Run creates databases the cleanup would then have to chase.
	//   2. The finalizeTimeout deadline self-cancels a wedged run instead of
	//      relying solely on the external FinalizeWatchdogLoop — a prepare that
	//      hangs inside an engine/network op stops pinning its slot (and the
	//      machine) the moment a phase boundary observes the expired ctx, plus
	//      every engine HTTP call inherits the deadline. The watchdog stays as
	//      the backstop that emits the error event + recovery if a phase never
	//      reaches a boundary.
	ctx, cancel := context.WithTimeout(ctx, finalizeTimeout)
	defer cancel()

	// Pin the caller's shell PATH onto ctx for every git subprocess
	// this finalize spawns — notably EnsureFilter's `git add
	// --renormalize`, which runs the clean filter (`treeman
	// patch-filter`). The async create tail and the file/HEAD watchers
	// reach here on st.BgCtx, which does NOT carry runOneTask's
	// override, so re-pin from inheritedEnv (the CLI-captured env,
	// rehydrated from the store on watcher-driven runs).
	ctx = gitcmd.WithPath(ctx, inheritedEnv["PATH"])

	// Dedup against concurrent finalize attempts on the same wtPath
	// — both the CLI's wt-create dispatch AND the lifecycle watcher
	// can race to call this for the same worktree when the watcher
	// is enabled. The watcher already gates on the active-row check,
	// but this is the belt to that braces: even if a second caller
	// slips through (e.g. an explicit `treeman wt finalize` while
	// one is still running), it returns immediately instead of
	// re-running setup hooks in parallel.
	if !st.MarkFinalizeInFlight(wtRoot, cancel) {
		return nil
	}
	defer st.UnmarkFinalizeInFlight(wtRoot)

	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wtRoot)
	if err != nil {
		return err
	}

	repoName := filepath.Base(repoRoot)
	repoID, err := st.Store.EnsureRepo(ctx, repoRoot, repoName)
	if err != nil {
		return err
	}

	// Main-wt routing — see wt.ResolveIdentity for the
	// two-source-of-truth check (cfg.MainWorktree.Enabled OR an
	// active is_main=1 row) that bridges the disable→reload race.
	// Without that bridge, a HEAD-watcher firing finalize between
	// `main disable` writing YAML and the reload soft-deleting the
	// row would create a fresh path-hash-keyed DB and orphan every
	// per-branch DB the main slug owns.
	id, err := wt.ResolveIdentity(ctx, st.Store, &cfg, repoRoot, wtRoot, detectBranch(wtRoot), repoID)
	if err != nil {
		return err
	}
	sl, wtID, isMain := id.Slug, id.WtID, id.IsMain
	termRepoID, termWtID = repoID, wtID

	// Cache the user's shell env per-worktree so daemon-driven re-
	// runs (HEAD watcher, file watcher) can rehydrate PATH etc.
	// FinalizeWorktree is reached from `wt create` / `wt finalize`
	// where the CLI captured os.Environ() and shipped it via RPC —
	// that's the canonical source for the worktree's env going
	// forward.
	if len(inheritedEnv) > 0 {
		_ = st.Store.SaveInheritedEnv(ctx, wtID, inheritedEnv)
	}

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateStart,
		"daemon-detached setup + prepare beginning",
		repoID, wtID, "", 0, nil)

	// Wait for `git worktree add` to finish checking out the tree
	// before firing hooks. Watcher-triggered finalize can race: the
	// new worktree dir + .git file land first, the daemon's filesystem
	// watcher sees them and dispatches finalize, but git is still
	// writing subdirs. A user `create-before-engines` hook with a
	// `cwd: frontend` then fails with `cd: can't cd to frontend`. CLI-
	// triggered finalize is already past the git wait so this is a
	// near-zero-cost no-op there. Bounded so a wedged checkout still
	// surfaces a clear error rather than blocking forever.
	if err := waitForCheckoutSettled(ctx, st, wtRoot, repoID, wtID, 60*time.Second); err != nil {
		return err
	}

	// Bring in worktrees.links / worktrees.copies. Offloaded from the CLI
	// (`wt create`) to here so the CLI returns immediately instead of
	// blocking on a recursive copy of e.g. a vendor/ tree. Idempotent —
	// existing destinations are skipped. Done before port alloc + patches
	// so the copied `.env` exists for the patch render below. Skipped for
	// the main worktree, which is the source the links/copies originate
	// from (src == dst), same rationale as patches.
	if !isMain {
		if err := bringInWithEvents(ctx, st, repoRoot, wtRoot, &cfg, repoID, wtID); err != nil {
			return err
		}
	}

	// Re-apply top-level patches: every finalize evaluates them
	// against the current HEAD's slug. Idempotent — Unchanged is a
	// no-op write, and EnsureFilter re-asserts the git clean/smudge
	// config + info/attributes whether or not the content changed
	// (re-run after `git pull` must re-enforce in case the user
	// fiddled with .git/config). Failures are logged but non-fatal
	// so a broken patch driver doesn't block the rest of finalize.
	//
	// Skipped for the main worktree: `patches:` describe how to derive
	// a per-worktree checkout (rewriting its `.env` DB name, ports,
	// etc.) from the canonical repo-root clone. The main checkout IS
	// that clone — rewriting its files would clobber the developer's
	// real `.env` (e.g. point `DB_DATABASE` at a per-branch name when
	// the main worktree's branch_scoped active DB is the bare,
	// overlay-resolved name the root `.env` already targets).
	// Ensure the declared port slots are allocated before patches/hooks
	// render `{{ port.* }}`. The CLI allocates at `wt create` time; an
	// external `git worktree add` reaches finalize via the lifecycle
	// watcher with no ports yet, so without this its patches/hooks would
	// see empty port values. Allocate is idempotent — a no-op when the
	// CLI already filled every slot. Skipped for the main worktree
	// (same rationale as patches: it IS the canonical clone).
	if len(cfg.Ports) > 0 && !isMain {
		allocatePortsWithEvents(ctx, st, &cfg, wtRoot, repoID, wtID)
	}

	if len(cfg.Patches) > 0 && !isMain {
		applyFinalizePatches(ctx, st, &cfg, wtRoot, sl, repoID, wtID)
	}

	// Three-step setup pipeline: create-before-engines actions →
	// engine prepare → create-after-engines actions. Each step waits
	// for the previous on the daemon side; the CLI never sees this
	// (it already returned).
	//
	// Phase boundaries double as cancellation checkpoints: a
	// concurrent TeardownWorktree fires `cancel` via CancelFinalize,
	// and the next check bails before prepare creates databases.
	done, err := runFinalizeSetupPipeline(ctx, st, &cfg, repoRoot, wtRoot, sl, isMain, repoID, wtID, inheritedEnv)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	// Start (or keep) the per-worktree fsnotify watcher so subsequent
	// migration edits inside the worktree trigger a prepare rerun.
	if err := startWorktreeWatcher(ctx, st, repoRoot, wtRoot); err != nil {
		slog.Warn("start worktree watcher", "wt", wtRoot, "err", err)
	} else {
		_ = st.Store.WriteEvent(ctx, store.LevelDebug, store.EvtWatchStart,
			"per-worktree fsnotify watcher active",
			repoID, wtID, "", 0, nil)
	}

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateEnd,
		"daemon-detached setup + prepare complete",
		repoID, wtID, "", 0, nil)
	return nil
}

// bringInWithEvents runs the link + copy bring-in passes for a non-main
// worktree, emitting <domain>:start / <domain>:path (debug) / <domain>:end
// per pass where <domain> is `copies` or `links`. Bring-in is the single
// biggest silent cost on create (a recursive copy of e.g. vendor/ or
// node_modules/ can run for tens of seconds); the per-entry line carries
// files + bytes + duration so a slow copy is attributable to a specific
// configured path. Extracted from FinalizeWorktree to keep that function
// under the complexity gate.
func bringInWithEvents(
	ctx context.Context,
	st *State,
	repoRoot, wtRoot string,
	cfg *config.Config,
	repoID, wtID int64,
) error {
	// startEvt/pathEvt/endEvt are the copies:* or links:* event types;
	// mode is the wt.BringInFilesReport verb (copy|link).
	emit := func(startEvt, pathEvt, endEvt, mode string, paths []string) error {
		if len(paths) == 0 {
			return nil
		}
		_ = st.Store.WriteEvent(ctx, store.LevelInfo, startEvt,
			fmt.Sprintf("entries=%d", len(paths)),
			repoID, wtID, "", 0, map[string]any{"entries": len(paths)})
		results, brErr := wt.BringInFilesReport(repoRoot, wtRoot, paths, mode, nil)
		var files, brought, skipped int
		var bytes, totalMs int64
		for _, r := range results {
			files += r.Files
			brought += r.Brought
			skipped += r.Skipped
			bytes += r.Bytes
			totalMs += r.DurationMs
			_ = st.Store.WriteEvent(ctx, store.LevelDebug, pathEvt,
				fmt.Sprintf("%s files=%d bytes=%d duration=%dms", r.Rel, r.Files, r.Bytes, r.DurationMs),
				repoID, wtID, "", r.DurationMs, map[string]any{
					"path": r.Rel, "matches": r.Matches,
					"brought": r.Brought, "skipped": r.Skipped, "missing": r.Missing,
					"files": r.Files, "bytes": r.Bytes,
				})
		}
		_ = st.Store.WriteEvent(ctx, store.LevelInfo, endEvt,
			fmt.Sprintf("entries=%d brought=%d skipped=%d files=%d bytes=%d", len(results), brought, skipped, files, bytes),
			repoID, wtID, "", totalMs, map[string]any{
				"entries": len(results), "brought": brought,
				"skipped": skipped, "files": files, "bytes": bytes,
			})
		return brErr
	}
	if err := emit(store.EvtLinksStart, store.EvtLinksPath, store.EvtLinksEnd, "link", cfg.Worktrees.Links); err != nil {
		return err
	}
	return emit(store.EvtCopiesStart, store.EvtCopiesPath, store.EvtCopiesEnd, "copy", cfg.Worktrees.Copies)
}

// allocatePortsWithEvents allocates the configured port slots for a
// non-main worktree, emitting wt_port_alloc on success (with the
// slot→port map) or wt_port_alloc_error on failure. Idempotent — a
// no-op when the CLI already filled every slot. Extracted from
// FinalizeWorktree to keep that function under the complexity gate.
func allocatePortsWithEvents(
	ctx context.Context,
	st *State,
	cfg *config.Config,
	wtRoot string,
	repoID, wtID int64,
) {
	allocs, err := ports.New().Allocate(ctx, st.Store, cfg, repoID, wtID)
	switch {
	case err != nil:
		slog.Warn("finalize: port allocation", "wt", wtRoot, "err", err)
		_ = st.Store.WriteEvent(ctx, store.LevelWarn, store.EvtPortsError,
			err.Error(), repoID, wtID, "", 0, nil)
	case len(allocs) > 0:
		slots := make(map[string]any, len(allocs))
		for _, a := range allocs {
			slots[a.Name] = a.Port
		}
		_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtPortsAllocate,
			fmt.Sprintf("allocated %d port slot(s)", len(allocs)),
			repoID, wtID, "", 0, map[string]any{"slots": slots})
	}
}

// applyFinalizePatches re-applies the top-level `patches:` against the
// current HEAD's slug and re-asserts the git clean/smudge filter.
// Idempotent; failures are logged but non-fatal. Extracted from
// FinalizeWorktree to keep that function under the complexity gate.
func applyFinalizePatches(
	ctx context.Context,
	st *State,
	cfg *config.Config,
	wtRoot string,
	sl slug.Slug,
	repoID, wtID int64,
) {
	portMap, _ := st.Store.LoadWorktreePorts(ctx, wtID)
	tplCtx := template.FromSlug(sl).WithPorts(portMap)
	files := make([]string, 0, len(cfg.Patches))
	for _, p := range cfg.Patches {
		files = append(files, p.File)
		res, err := patcher.Apply(p, wtRoot, tplCtx)
		if err != nil {
			slog.Warn("patch failed", "wt", wtRoot, "file", p.File, "err", err)
			continue
		}
		if res.Outcome == patcher.Updated {
			_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtPatchesApply,
				fmt.Sprintf("driver=%s file=%s", res.Driver, res.File),
				repoID, wtID, "", 0, map[string]string{
					"driver": res.Driver,
					"file":   res.File,
				})
		}
	}
	if err := patcher.EnsureFilter(ctx, wtRoot, files); err != nil {
		slog.Warn("install patch filter", "wt", wtRoot, "err", err)
	}
}

// runFinalizeSetupPipeline runs the three-step setup pipeline:
// create-before-engines actions → engine prepare →
// create-after-engines actions. Each phase boundary is a
// cancellation checkpoint: when a concurrent TeardownWorktree fires
// `cancel`, the next check writes a worktree:create:cancel event and
// returns (done=true, nil) so the caller stops short of starting the
// watcher / emitting the done event. A non-nil error aborts the
// pipeline. Extracted from FinalizeWorktree to keep that function
// under the cyclomatic-complexity gate.
func runFinalizeSetupPipeline(
	ctx context.Context,
	st *State,
	cfg *config.Config,
	repoRoot, wtRoot string,
	sl slug.Slug,
	isMain bool,
	repoID, wtID int64,
	inheritedEnv map[string]string,
) (done bool, err error) {
	if cancelledBefore(ctx, st, "pre-hooks", repoID, wtID) {
		return true, nil
	}
	if err := runTriggerActions(ctx, st, "create-before-engines",
		cfg.Hooks.OnCreateBeforeEngines, repoRoot, wtRoot, sl.Value,
		isMain, repoID, wtID, inheritedEnv); err != nil {
		return false, err
	}
	if cancelledBefore(ctx, st, "prepare", repoID, wtID) {
		return true, nil
	}
	if len(cfg.Databases) > 0 {
		_, prepErr := prepare.Run(ctx, cfg, wtRoot, sl, st.Store, repoID, wtID, inheritedEnv)
		// A cancelled context means a concurrent teardown preempted
		// prepare — a clean stop regardless of whether prepare.Run
		// surfaced an error on the way out. Mirrors the un-extracted
		// FinalizeWorktreeForWatch path; nilerr can't tell this
		// ctx-cancellation guard apart from a swallowed prepare error.
		if ctx.Err() != nil {
			return true, nil //nolint:nilerr // cancellation is a clean stop, not an error
		}
		if prepErr != nil {
			return false, fmt.Errorf("prepare: %w", prepErr)
		}
	}
	if cancelledBefore(ctx, st, "post-hooks", repoID, wtID) {
		return true, nil
	}
	if err := runTriggerActions(ctx, st, "create-after-engines",
		cfg.Hooks.OnCreateAfterEngines, repoRoot, wtRoot, sl.Value,
		isMain, repoID, wtID, inheritedEnv); err != nil {
		return false, err
	}
	return false, nil
}

// cancelledBefore is a phase-boundary cancellation checkpoint: when a
// concurrent TeardownWorktree has fired `cancel`, it records a
// worktree:create:cancel event naming the phase about to be skipped and
// reports true so the pipeline bails. Isolating the ctx.Err() check
// here keeps the "cancelled, not an error" return out of the call
// site's error-handling flow.
func cancelledBefore(ctx context.Context, st *State, phase string, repoID, wtID int64) bool {
	if ctx.Err() == nil {
		return false
	}
	_ = st.Store.WriteEvent(ctx, store.LevelWarn, store.EvtWorktreeCreateCancel,
		"finalize aborted before "+phase, repoID, wtID, "", 0, nil)
	return true
}

// FinalizeWorktreeForWatch is the watcher-driven re-prepare path.
// Called when a file matching one of the owning database's
// `inputs:` globs changes. Re-applies patches (cheap, idempotent)
// and re-runs prepare scoped to the matched database. The cache-
// hit / cold-build decision is derived purely from the input hash
// fingerprint — no force-rebuild override.
//
// `dbIdx == -1` means the matched glob was top-level / applies to
// every database; in that case every DB re-prepares.
//
// Setup hooks are NOT re-run by this path — they're for
// initial worktree setup, not edit-driven re-prep. Use the regular
// `FinalizeWorktree` (e.g. `treeman wt finalize`) when hooks need
// to fire.
func FinalizeWorktreeForWatch(
	ctx context.Context,
	st *State,
	repoPath, worktreePath string,
	dbIdx int,
	inheritedEnv map[string]string,
) error {
	repoRoot := repoPath
	wtRoot := worktreePath

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Same PATH pin as FinalizeWorktree — this watcher path re-applies
	// patches (EnsureFilter → `git add --renormalize` → clean filter)
	// and runs on st.BgCtx with no inherited override.
	ctx = gitcmd.WithPath(ctx, inheritedEnv["PATH"])
	if !st.MarkFinalizeInFlight(wtRoot, cancel) {
		return nil
	}
	defer st.UnmarkFinalizeInFlight(wtRoot)

	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wtRoot)
	if err != nil {
		return err
	}
	repoName := filepath.Base(repoRoot)
	repoID, err := st.Store.EnsureRepo(ctx, repoRoot, repoName)
	if err != nil {
		return err
	}
	// Routing through the same helper as FinalizeWorktree so a
	// watcher-driven re-prepare on the main worktree applies the
	// per-context overlay + uses the main_<branch> slug. Without
	// this, a touch on a watched migration in the repo root would
	// rebuild against a path-hash-keyed DB while the rest of the
	// system addresses main_<branch>.
	id, err := wt.ResolveIdentity(ctx, st.Store, &cfg, repoRoot, wtRoot, detectBranch(wtRoot), repoID)
	if err != nil {
		return err
	}
	sl, wtID := id.Slug, id.WtID

	// Re-apply patches. Cheap; idempotent for unchanged content.
	// Skipped for the main worktree — see FinalizeWorktree for why
	// the canonical clone's files are left untouched.
	if len(cfg.Patches) > 0 && !id.IsMain {
		portMap, _ := st.Store.LoadWorktreePorts(ctx, wtID)
		tplCtx := template.FromSlug(sl).WithPorts(portMap)
		for _, p := range cfg.Patches {
			if _, err := patcher.Apply(p, wtRoot, tplCtx); err != nil {
				slog.Warn("patch failed (watch-trigger)", "wt", wtRoot, "file", p.File, "err", err)
			}
		}
	}

	if len(cfg.Databases) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	// The fingerprint hashes every declared input + dump + the
	// migrate/seed run-strings. If anything changed, prepare cold-
	// builds; otherwise cache-hits. No separate force-rebuild knob.
	opts := prepare.RunOptions{}
	if dbIdx >= 0 && dbIdx < len(cfg.Databases) {
		opts.FilterDBs = true
		opts.OnlyDBIndex = dbIdx
	}
	if _, err := prepare.RunFiltered(ctx, &cfg, wtRoot, sl, st.Store, repoID, wtID, inheritedEnv, opts); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("prepare (watch-trigger): %w", err)
	}
	return nil
}

// TeardownWorktree mirrors FinalizeWorktree for `treeman wt delete`.
// Runs teardown hooks + DB teardown + worktree removal. All in the
// daemon's runtime so the CLI returns immediately.
//
// Mutex scope: the per-repo teardown mutex is held ONLY across the
// git operations that touch the shared <common>/worktrees/ admin
// directory. Teardown hooks and DROP DATABASE are per-worktree
// work — running them in parallel for two simultaneous teardowns of
// the same repo is fine, and shrinking the mutex window lets a
// second `gwtd` start its teardown/DB drops without waiting for the
// first's rm to finish.
//
// Heavy rm-rf is offloaded to a background reaper: the worktree is
// atomically renamed into a trash directory on the same filesystem
// (O(1)), then `git worktree prune` unregisters the admin entry,
// then a detached goroutine deletes the trashed tree at SCHED_IDLE +
// ionice idle class. The user-visible teardown finishes in
// milliseconds even on a Laravel vendor+node_modules checkout.
//
// Watchers die FIRST — before the per-repo mutex, before any config
// load. The in-flight marker tells the lifecycle watcher to skip
// spawning an orphan teardown when its admin-dir REMOVE event fires
// during the git prune step.
func TeardownWorktree(
	ctx context.Context,
	st *State,
	repoPath, worktreePath string,
	force bool,
	inheritedEnv map[string]string,
) error {
	repoRoot := repoPath
	wtRoot := worktreePath

	st.UnregisterWtWatcher(wtRoot)
	if !st.MarkTeardownInFlight(wtRoot) {
		return nil
	}
	defer st.UnmarkTeardownInFlight(wtRoot)

	// Preempt any in-flight FinalizeWorktree for this path so its
	// late-arriving prepare doesn't resurrect databases or the
	// registry row after teardown completes. Then block until the
	// finalize goroutine has actually exited (its phase boundaries
	// observe ctx.Err() and bail) before we read state and start
	// dropping things. The wait is generous because setup hooks can
	// be slow (npm install, composer install) and the cancel only
	// takes effect at the next phase boundary.
	if st.CancelFinalize(wtRoot) {
		slog.Info("teardown: cancelled in-flight finalize", "wt", wtRoot)
		if err := st.WaitFinalizeCleared(ctx, wtRoot, 5*time.Minute); err != nil {
			slog.Warn("teardown: finalize did not clear in time, proceeding anyway",
				"wt", wtRoot, "err", err)
		}
	}

	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wtRoot)
	if err != nil {
		return err
	}

	repoID, err := st.Store.LookupRepoID(ctx, repoRoot)
	if err != nil {
		return err
	}
	row, err := lookupWorktreeByPath(ctx, st.Store, wtRoot)
	if err != nil {
		return err
	}
	if row.ID == 0 {
		// No DB row at all — nothing to tear down from treeman's
		// side. Still honour --force by letting git reap a stale
		// checkout if one happens to remain on disk.
		if force {
			mu := st.LockRepoTeardown(repoPath)
			mu.Lock()
			gitArgs := []string{"-C", repoRoot, "worktree", "remove", "--force", wtRoot}
			_ = lowPriorityCommand(ctx, "git", gitArgs).Run()
			pruneEmptyParentsBelow(wtRoot, worktreesRootOf(cfg.Worktrees.Root, repoRoot))
			mu.Unlock()
		}
		return nil
	}
	if row.Deleted {
		// Row already marked deleted. Two sub-cases:
		//   - working tree gone too → fully cleaned up, no-op.
		//   - working tree still on disk → orphan from an earlier
		//     aborted teardown (or from before the EnsureWorktree
		//     resurrection fix). Run the full sequence anyway —
		//     every step below is idempotent so the second pass
		//     just finishes what the first one didn't.
		if _, statErr := os.Stat(wtRoot); statErr != nil {
			return nil
		}
		slog.Info("teardown: orphan recovery — row deleted but working tree on disk",
			"wt", wtRoot, "id", row.ID)
	}
	wtID := row.ID
	slugVal := row.Slug
	worktreesRoot := worktreesRootOf(cfg.Worktrees.Root, repoRoot)

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeDeleteStart,
		"daemon-detached teardown hooks + db teardown + git remove beginning",
		repoID, wtID, "", 0, nil)

	// Three-step teardown: delete-before-engines actions →
	// engine drop (inline + synchronous so post-engine actions can
	// observe the drop) → delete-after-engines actions. Drop
	// failures are logged but don't abort the rest.
	_ = runTriggerActions(ctx, st, "delete-before-engines",
		cfg.Hooks.OnDeleteBeforeEngines, repoRoot, wtRoot, slugVal,
		row.IsMain, repoID, wtID, inheritedEnv)
	if len(cfg.Databases) > 0 {
		if err := prepare.TeardownDatabases(ctx, &cfg, slugVal, repoID, wtID, st.Store); err != nil {
			slog.Warn("teardown DB drop", "wt", wtRoot, "err", err)
		}
	}
	_ = runTriggerActions(ctx, st, "delete-after-engines",
		cfg.Hooks.OnDeleteAfterEngines, repoRoot, wtRoot, slugVal,
		row.IsMain, repoID, wtID, inheritedEnv)

	// Acquire the per-repo mutex only for the git operations — they
	// touch the shared <common>/worktrees/ admin tree and aren't
	// parallel-safe.
	mu := st.LockRepoTeardown(repoPath)
	mu.Lock()
	trashPath, removeErr := removeWorktreeViaTrash(ctx, repoRoot, wtRoot, worktreesRoot, force)
	pruneEmptyParentsBelow(wtRoot, worktreesRoot)
	mu.Unlock()

	if removeErr != nil && !force {
		return fmt.Errorf("worktree remove: %w (pass --force to override)", removeErr)
	}

	// Physically drop the port reservations so the freed ports can be
	// re-used by the next allocation. Soft-deleting the worktree row is
	// not enough: ListUsedPorts skips them, but the unique index on
	// (repo_id, name, port) still rejects the re-insert, so the freed
	// port silently climbs out of the range. Mirror the CLI inline path.
	_ = st.Store.ReleaseWorktreePorts(ctx, wtID)
	// Reap every active-branch marker too, so a worktree later created at
	// the same path starts clean (also clears markers for databases since
	// removed from config).
	_ = st.Store.ClearActiveBranchesForWorktree(ctx, wtID)

	_ = st.Store.MarkWorktreeDeleted(ctx, wtID)
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeDeleteEnd,
		"daemon-detached teardown complete",
		repoID, wtID, "", 0, nil)

	if trashPath != "" {
		scheduleBackgroundReap(st, repoRoot, trashPath)
	}
	return nil
}

// worktreesRootOf resolves the configured worktrees.root (default
// `.worktrees`) against the main repo. Mirrors resolveWorktreesRoot
// in cmd/treeman/cmd — duplicated here so the daemon doesn't need to
// import the CLI package.
func worktreesRootOf(raw, repoRoot string) string {
	if raw == "" {
		raw = ".worktrees"
	}
	if filepath.IsAbs(raw) {
		return raw
	}
	return filepath.Join(repoRoot, raw)
}

// pruneEmptyParentsBelow walks up from `start` removing now-empty
// directories until we leave `wtRoot`. Mirrors the CLI helper —
// daemon-side teardown needs the same cleanup so `.worktrees/feature/`
// doesn't linger after the leaf is removed.
func pruneEmptyParentsBelow(start, wtRoot string) {
	if start == "" || wtRoot == "" {
		return
	}
	parent := filepath.Dir(start)
	for {
		rel, err := filepath.Rel(wtRoot, parent)
		if err != nil || rel == "." || rel == ".." || len(rel) >= 3 && rel[:3] == "../" {
			return
		}
		if err := os.Remove(parent); err != nil {
			return
		}
		parent = filepath.Dir(parent)
	}
}

// lowPriorityCommand wraps an exec.CommandContext invocation so the
// child runs at low CPU + I/O + scheduler priority. Used for the
// background rm reaper and the rare git-worktree-remove fallback —
// `rm -rf` of vendor/ + node_modules/ is ~250k inode unlinks back-
// to-back, which without these wrappers can starve foreground apps
// long enough to look like a system freeze.
//
// Layering, outermost first: `chrt -i 0 ionice -c 3 nice -n 19 <cmd>`.
//   - `chrt -i 0` puts the process in SCHED_IDLE — Linux only,
//     deeper than `nice -n 19` (SCHED_IDLE only runs when *nothing*
//     else wants CPU, including other niced processes).
//   - `ionice -c 3` is the idle I/O class — disk bandwidth only when
//     nothing else wants it.
//   - `nice -n 19` is the lowest CPU priority (used as a fallback on
//     systems without chrt).
//
// Each layer is optional: when the binary is missing we drop it and
// keep the rest. macOS shells usually carry `nice` but not `ionice`
// or `chrt`, so the nice-only fallback still gives partial relief.
func lowPriorityCommand(ctx context.Context, name string, args []string) *exec.Cmd {
	var prefix []string
	if p, err := exec.LookPath("chrt"); err == nil {
		prefix = append(prefix, p, "-i", "0")
	}
	if p, err := exec.LookPath("ionice"); err == nil {
		prefix = append(prefix, p, "-c", "3")
	}
	if p, err := exec.LookPath("nice"); err == nil {
		prefix = append(prefix, p, "-n", "19")
	}
	if len(prefix) == 0 {
		return exec.CommandContext(ctx, name, args...)
	}
	// First prefix element becomes the executable; the rest of the
	// prefix + the original (name, args) become its arguments.
	cmdArgs := append([]string{}, prefix[1:]...)
	cmdArgs = append(cmdArgs, name)
	cmdArgs = append(cmdArgs, args...)
	return exec.CommandContext(ctx, prefix[0], cmdArgs...)
}

// runTriggerActions dispatches one trigger's actions (parallel
// groups; the daemon blocks on completion before returning, but the
// CLI dispatched-and-returned long ago). No-op when the trigger has
// no actions. Errors are returned so the caller can decide whether
// to abort the pipeline.
func runTriggerActions(
	ctx context.Context,
	st *State,
	trigger string,
	actions []config.Action,
	repoRoot, wtRoot, slugVal string,
	isMain bool,
	repoID, wtID int64,
	inheritedEnv map[string]string,
) error {
	if len(actions) == 0 {
		return nil
	}
	started := hooks.EmitHookStart(ctx, st.Store, repoID, wtID, trigger, len(actions))
	out, err := hooks.RunHooks(ctx, trigger, actions, repoRoot, wtRoot, slugVal, isMain, inheritedEnv, true)
	runIDs := hooks.PersistOutcome(ctx, st.Store, repoID, wtID, trigger, started, nowMillis(), out)
	if err != nil {
		return fmt.Errorf("%s: %w", trigger, err)
	}
	// RunHooks records non-zero child exits on the outcome but does not
	// itself error (it leaves the decision to the caller). For the
	// create pipeline a failed phase must abort: the next phase
	// (engine prepare / migrate) depends on this one having populated
	// vendor/ etc., so letting it proceed only produces a confusing
	// downstream failure. Abort here with the real cause.
	if failErr := hooks.FirstFailureError(trigger, out, runIDs); failErr != nil {
		return failErr
	}
	return nil
}

// detectBranch reads `.git/HEAD` (or the gitlink-resolved file) and
// returns the branch name, empty when detached or readable.
func detectBranch(worktree string) string {
	headPath := filepath.Join(worktree, ".git", "HEAD")
	info, err := os.Stat(headPath)
	if err != nil || info.IsDir() {
		// gitlink case (linked worktrees use .git as a file).
		linkBytes, err := os.ReadFile(filepath.Join(worktree, ".git"))
		if err != nil {
			return ""
		}
		gitdir, _ := strings.CutPrefix(string(linkBytes), "gitdir: ")
		headPath = filepath.Join(strings.TrimSpace(gitdir), "HEAD")
	}
	b, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	if ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/"); ok {
		return ref
	}
	return ""
}

// waitForCheckoutSettled blocks until every top-level entry recorded in
// HEAD exists on disk inside wtRoot, or `timeout` elapses. Run before
// firing setup hooks so a hook with a `cwd: <subdir>` action never
// races a still-running `git worktree add` that hasn't reached that
// subdir yet. CLI-triggered finalize already completed git checkout
// before this point, so the first probe succeeds instantly; the wait
// only kicks in on the watcher-triggered path.
//
// Always emits a `worktree:create:checkout` event so the stage is never invisible:
// at info level when the wait was non-trivial (>50ms) so a slow checkout
// stands out, at debug otherwise (the instant CLI-triggered path).
func waitForCheckoutSettled(ctx context.Context, st *State, wtRoot string, repoID, wtID int64, timeout time.Duration) error {
	started := time.Now()
	deadline := started.Add(timeout)
	// HEAD's top-level entry list is fixed for the duration of the wait,
	// so the `git ls-tree` subprocess runs at most once (after .git
	// appears); every later iteration is stat-only.
	var (
		entries     []string
		haveEntries bool
	)
	for {
		if !haveEntries {
			entries, haveEntries = checkoutEntries(ctx, wtRoot)
		}
		if haveEntries && entriesPresent(wtRoot, entries) {
			waited := time.Since(started)
			level := store.LevelDebug
			if waited > 50*time.Millisecond {
				level = store.LevelInfo
			}
			_ = st.Store.WriteEvent(ctx, level, store.EvtWorktreeCreateCheckout,
				fmt.Sprintf("git worktree add settled after %dms", waited.Milliseconds()),
				repoID, wtID, "", waited.Milliseconds(), map[string]any{
					"waited_ms": waited.Milliseconds(),
				})
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("git worktree add did not settle in %s; top-level HEAD entries missing under %s", timeout, wtRoot)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(75 * time.Millisecond):
		}
	}
}

// checkoutEntries lists HEAD's top-level paths for wtRoot. ok is false
// while .git is still missing (worktree add hasn't started writing);
// once .git exists the result is cacheable — an unborn HEAD /
// pre-commit repo yields (nil, true), meaning nothing to wait for.
func checkoutEntries(ctx context.Context, wtRoot string) (entries []string, ok bool) {
	if _, err := os.Stat(filepath.Join(wtRoot, ".git")); err != nil {
		return nil, false
	}
	out, err := exec.CommandContext(ctx, "git", "-C", wtRoot, "ls-tree", "--name-only", "HEAD").Output()
	if err != nil {
		// Unborn HEAD / pre-commit repo: nothing to wait for.
		return nil, true
	}
	for name := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if name != "" {
			entries = append(entries, name)
		}
	}
	return entries, true
}

// entriesPresent reports whether every top-level path in HEAD exists
// on disk inside wtRoot. `git worktree add` writes the entries in tree
// order, so the LAST one to appear is the latest top-level entry — once
// every one is present the working tree is materialized.
func entriesPresent(wtRoot string, entries []string) bool {
	for _, name := range entries {
		if _, err := os.Stat(filepath.Join(wtRoot, name)); err != nil {
			return false
		}
	}
	return true
}
