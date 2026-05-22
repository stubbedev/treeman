package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// FinalizeWorktree is the daemon's tokio-equivalent (just a Go
// goroutine) tail of `treeman wt create` when
// `worktrees.async_create` is true. Runs postcreate hooks + prepare
// against the main repo root.
func FinalizeWorktree(
	ctx context.Context,
	st *State,
	repoPath, worktreePath string,
	inheritedEnv map[string]string,
) error {
	repoRoot := repoPath
	wtRoot := worktreePath

	// Dedup against concurrent finalize attempts on the same wtPath
	// — both the CLI's wt-create dispatch AND the lifecycle watcher
	// can race to call this for the same worktree when the watcher
	// is enabled. The watcher already gates on the active-row check,
	// but this is the belt to that braces: even if a second caller
	// slips through (e.g. an explicit `treeman wt finalize` while
	// one is still running), it returns immediately instead of
	// re-running postcreate hooks in parallel.
	if !st.MarkFinalizeInFlight(wtRoot) {
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

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_finalize_start",
		"daemon-detached postcreate + prepare beginning",
		repoID, wtID, "", 0, nil)

	if len(cfg.Hooks.Postcreate) > 0 {
		// Block until every postcreate group exits before kicking off
		// prepare. Framework migrate commands (Laravel artisan, Rails
		// rake, Django manage.py) read `vendor/` / `node_modules/` /
		// the venv that a postcreate `composer install` / `yarn
		// install` / `pip install` populates. Firing prepare in
		// parallel with hooks races against an empty vendor dir and
		// blows up with `migrate exit 255`. Hook groups still run in
		// parallel with each other; only the phase-to-phase
		// transition is gated.
		_, err := hooks.RunHooks(ctx, "postcreate", cfg.Hooks.Postcreate,
			repoRoot, wtRoot, sl.Value, inheritedEnv, true)
		if err != nil {
			return fmt.Errorf("postcreate hooks: %w", err)
		}
	}

	if len(cfg.Databases) > 0 {
		if _, err := prepare.Run(ctx, &cfg, wtRoot, sl, st.Store, repoID, wtID, inheritedEnv); err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
	}

	// Start (or keep) the per-worktree fsnotify watcher so subsequent
	// migration edits inside the worktree trigger a prepare rerun.
	if err := startWorktreeWatcher(ctx, st, repoRoot, wtRoot); err != nil {
		slog.Warn("start worktree watcher", "wt", wtRoot, "err", err)
	}

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_finalize_done",
		"daemon-detached postcreate + prepare complete",
		repoID, wtID, "", 0, nil)
	return nil
}

// TeardownWorktree mirrors FinalizeWorktree for `treeman wt delete`.
// Runs predelete hooks + DB teardown + worktree removal. All in the
// daemon's runtime so the CLI returns immediately.
//
// Mutex scope: the per-repo teardown mutex is held ONLY across the
// git operations that touch the shared <common>/worktrees/ admin
// directory. Predelete hooks and DROP DATABASE are per-worktree
// work — running them in parallel for two simultaneous teardowns of
// the same repo is fine, and shrinking the mutex window lets a
// second `gwtd` start its predelete/DB drops without waiting for the
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
		"daemon-detached predelete + db teardown + git remove beginning",
		repoID, wtID, "", 0, nil)

	if len(cfg.Hooks.Predelete) > 0 {
		// Block until every predelete group exits before dropping
		// databases — same rationale as the postcreate await: a hook
		// that closes app connections must finish before DROP
		// DATABASE, or the DB server rejects the drop.
		_, _ = hooks.RunHooks(ctx, "predelete", cfg.Hooks.Predelete,
			repoRoot, wtRoot, slugVal, inheritedEnv, true)
	}

	if len(cfg.Databases) > 0 {
		// DROP DATABASE is the second source of "wt delete locks the
		// host" complaints — on MySQL+InnoDB the ibd unlinks for a
		// large schema can stall the server for seconds. Same pattern
		// as the file reaper: queue the drop on a per-repo worker so
		// the RPC caller returns immediately and concurrent gwtds on
		// the same repo serialise drops instead of fanning out.
		ScheduleDBDrop(st, repoRoot, DBDropJob{
			Cfg:        cfg,
			Slug:       slugVal,
			RepoID:     repoID,
			WorktreeID: wtID,
		})
	}

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
		gitdir := stripPrefix(string(linkBytes), "gitdir: ")
		gitdir = trimSpace(gitdir)
		headPath = filepath.Join(gitdir, "HEAD")
	}
	b, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	line := trimSpace(string(b))
	const prefix = "ref: refs/heads/"
	if len(line) > len(prefix) && line[:len(prefix)] == prefix {
		return line[len(prefix):]
	}
	return ""
}

func stripPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
