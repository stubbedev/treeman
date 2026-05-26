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
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/patcher"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

func nowMillis() int64 { return time.Now().UnixMilli() }

// FinalizeWorktree is the daemon's tokio-equivalent (just a Go
// goroutine) tail of `treeman wt create` when
// `worktrees.async_create` is true. Runs setup hooks + prepare
// against the main repo root.
func FinalizeWorktree(
	ctx context.Context,
	st *State,
	repoPath, worktreePath string,
	inheritedEnv map[string]string,
) error {
	repoRoot := repoPath
	wtRoot := worktreePath

	// Derive a cancellable ctx that TeardownWorktree can preempt via
	// CancelFinalize. ctx.Err() is consulted at each phase boundary
	// below so a concurrent `wt delete` preempts before prepare.Run
	// creates databases the cleanup would then have to chase.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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
	branch := detectBranch(wtRoot)

	repoName := filepath.Base(repoRoot)
	repoID, err := st.Store.EnsureRepo(ctx, repoRoot, repoName)
	if err != nil {
		return err
	}

	// Main-wt routing. Two-source-of-truth check by design:
	//
	//   1. `cfg.MainWorktree.Enabled` — the live config says use main.
	//   2. An active row with is_main=1 — the registry has a main row.
	//
	// Either is enough. The row-fallback covers the brief window
	// between `treeman main disable` writing the YAML and the
	// config-reload soft-deleting the row: if a HEAD-watcher fires
	// finalize during that window, we MUST keep using slug.ForMain
	// against the same main row, otherwise the finalize would create
	// a fresh path-hash-keyed DB and orphan every per-branch DB the
	// main slug owns. Once the reload soft-deletes the row,
	// LookupMainWorktree returns 0 and subsequent finalizes route
	// back to the linked-wt path (slug.For).
	isMain := false
	if wtRoot == repoRoot {
		if cfg.MainWorktree.Enabled {
			isMain = true
		} else if existing, err := st.Store.LookupMainWorktree(ctx, repoID); err == nil && existing.ID != 0 {
			isMain = true
		}
	}

	var sl slug.Slug
	if isMain {
		sl = slug.ForMain(wtRoot, branch)
		// Apply the per-context overlay so prepare.Run + hook
		// rendering see the main-wt-specific NameTemplate /
		// TestClones / Fanout fields. Linked-wt finalizes skip this
		// — their cfg stays as-loaded.
		config.ApplyMainWorktreeOverlay(&cfg)
	} else {
		sl = slug.For(wtRoot, branch)
	}

	var wtID int64
	if isMain {
		wtID, err = st.Store.EnsureMainWorktree(ctx, repoID, wtRoot, sl.Value, branch)
	} else {
		wtID, err = st.Store.EnsureWorktree(ctx, repoID, wtRoot, sl.Value, branch)
	}
	if err != nil {
		return err
	}

	// Cache the user's shell env per-worktree so daemon-driven re-
	// runs (HEAD watcher, file watcher) can rehydrate PATH etc.
	// FinalizeWorktree is reached from `wt create` / `wt finalize`
	// where the CLI captured os.Environ() and shipped it via RPC —
	// that's the canonical source for the worktree's env going
	// forward.
	if len(inheritedEnv) > 0 {
		_ = st.Store.SaveInheritedEnv(ctx, wtID, inheritedEnv)
	}

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_finalize_start",
		"daemon-detached setup + prepare beginning",
		repoID, wtID, "", 0, nil)

	// Re-apply top-level patches: every finalize evaluates them
	// against the current HEAD's slug. Idempotent — Unchanged is a
	// no-op write, and the skip-worktree bit is re-asserted whether
	// or not the content changed (re-run after `git pull` must
	// re-enforce). Failures are logged but non-fatal so a broken
	// patch driver doesn't block the rest of finalize.
	if len(cfg.Patches) > 0 {
		tplCtx := template.FromSlug(sl)
		for _, p := range cfg.Patches {
			res, err := patcher.Apply(p, wtRoot, tplCtx)
			if err != nil {
				slog.Warn("patch failed", "wt", wtRoot, "file", p.File, "err", err)
				continue
			}
			if res.Outcome == patcher.Updated {
				_ = st.Store.WriteEvent(ctx, store.LevelInfo, "patch_applied",
					fmt.Sprintf("driver=%s file=%s", res.Driver, res.File),
					repoID, wtID, "", 0, map[string]string{
						"driver": res.Driver,
						"file":   res.File,
					})
			}
		}
	}

	// Three-step setup pipeline: on-create-before-engines actions →
	// engine prepare → on-create-after-engines actions. Each step waits
	// for the previous on the daemon side; the CLI never sees this
	// (it already returned).
	//
	// Phase boundaries double as cancellation checkpoints: a
	// concurrent TeardownWorktree fires `cancel` via CancelFinalize,
	// and the next check bails before prepare creates databases.
	if err := ctx.Err(); err != nil {
		_ = st.Store.WriteEvent(ctx, store.LevelWarn, "wt_finalize_cancelled",
			"finalize aborted before pre-hooks", repoID, wtID, "", 0, nil)
		return nil
	}
	if err := runTriggerActions(ctx, st, "on-create-before-engines",
		cfg.Hooks.OnCreateBeforeEngines, repoRoot, wtRoot, sl.Value,
		repoID, wtID, inheritedEnv); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = st.Store.WriteEvent(ctx, store.LevelWarn, "wt_finalize_cancelled",
			"finalize aborted before prepare", repoID, wtID, "", 0, nil)
		return nil
	}
	if len(cfg.Databases) > 0 {
		if _, err := prepare.Run(ctx, &cfg, wtRoot, sl, st.Store, repoID, wtID, inheritedEnv); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("prepare: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = st.Store.WriteEvent(ctx, store.LevelWarn, "wt_finalize_cancelled",
			"finalize aborted before post-hooks", repoID, wtID, "", 0, nil)
		return nil
	}
	if err := runTriggerActions(ctx, st, "on-create-after-engines",
		cfg.Hooks.OnCreateAfterEngines, repoRoot, wtRoot, sl.Value,
		repoID, wtID, inheritedEnv); err != nil {
		return err
	}

	// Start (or keep) the per-worktree fsnotify watcher so subsequent
	// migration edits inside the worktree trigger a prepare rerun.
	if err := startWorktreeWatcher(ctx, st, repoRoot, wtRoot); err != nil {
		slog.Warn("start worktree watcher", "wt", wtRoot, "err", err)
	}

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_finalize_done",
		"daemon-detached setup + prepare complete",
		repoID, wtID, "", 0, nil)
	return nil
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
	if !st.MarkFinalizeInFlight(wtRoot, cancel) {
		return nil
	}
	defer st.UnmarkFinalizeInFlight(wtRoot)

	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wtRoot)
	if err != nil {
		return err
	}
	branch := detectBranch(wtRoot)
	sl := slug.For(wtRoot, branch)
	repoName := filepath.Base(repoRoot)
	repoID, err := st.Store.EnsureRepo(ctx, repoRoot, repoName)
	if err != nil {
		return err
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, wtRoot, sl.Value, branch)
	if err != nil {
		return err
	}

	// Re-apply patches. Cheap; idempotent for unchanged content.
	if len(cfg.Patches) > 0 {
		tplCtx := template.FromSlug(sl)
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

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_teardown_start",
		"daemon-detached teardown hooks + db teardown + git remove beginning",
		repoID, wtID, "", 0, nil)

	// Three-step teardown: on-delete-before-engines actions →
	// engine drop (inline + synchronous so post-engine actions can
	// observe the drop) → on-delete-after-engines actions. Drop
	// failures are logged but don't abort the rest.
	_ = runTriggerActions(ctx, st, "on-delete-before-engines",
		cfg.Hooks.OnDeleteBeforeEngines, repoRoot, wtRoot, slugVal,
		repoID, wtID, inheritedEnv)
	if len(cfg.Databases) > 0 {
		if err := prepare.TeardownDatabases(ctx, &cfg, slugVal, repoID, wtID, st.Store); err != nil {
			slog.Warn("teardown DB drop", "wt", wtRoot, "err", err)
		}
	}
	_ = runTriggerActions(ctx, st, "on-delete-after-engines",
		cfg.Hooks.OnDeleteAfterEngines, repoRoot, wtRoot, slugVal,
		repoID, wtID, inheritedEnv)

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

	_ = st.Store.MarkWorktreeDeleted(ctx, wtID)
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_teardown_done",
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
	repoID, wtID int64,
	inheritedEnv map[string]string,
) error {
	if len(actions) == 0 {
		return nil
	}
	started := hooks.EmitHookStart(ctx, st.Store, repoID, wtID, trigger, len(actions))
	out, err := hooks.RunHooks(ctx, trigger, actions, repoRoot, wtRoot, slugVal, inheritedEnv, true)
	hooks.PersistOutcome(ctx, st.Store, repoID, wtID, trigger, started, nowMillis(), out)
	if err != nil {
		return fmt.Errorf("%s: %w", trigger, err)
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
