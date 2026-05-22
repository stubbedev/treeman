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
// Runs predelete hooks + DB teardown + `git worktree remove`. All
// in the daemon's runtime so the CLI returns immediately.
//
// Serialised per repo via st.LockRepoTeardown — two fast-fire
// teardown invocations on the same repo queue instead of running
// concurrent `git worktree remove --force` + DROP DATABASE storms.
// The mutex is held for the entire goroutine; this is intentional. A
// single teardown on a Laravel vendor+node_modules tree is heavy
// enough on its own; doubling it can lock the host.
//
// Watchers die FIRST — before the per-repo mutex, before any config
// load. `git worktree remove --force` rm -rf's the checkout; if the
// per-worktree fsnotify watcher is still draining events from that
// rm storm, each match queues a dispatch that fires FinalizeWorktree
// for the worktree being deleted (re-creating DB resources mid-
// teardown). The in-flight marker also tells the lifecycle watcher
// to skip spawning an orphan teardown when its admin-dir REMOVE
// event fires.
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

	mu := st.LockRepoTeardown(repoPath)
	mu.Lock()
	defer mu.Unlock()

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
	if row.ID == 0 || row.Deleted {
		// Unregistered / already-deleted row — nothing to tear down
		// from treeman's side. Still honour --force by letting git
		// reap a stale checkout if one happens to remain on disk.
		if force {
			gitArgs := []string{"-C", repoRoot, "worktree", "remove", "--force", wtRoot}
			_ = lowPriorityCommand(ctx, "git", gitArgs).Run()
			pruneEmptyParentsBelow(wtRoot, worktreesRootOf(cfg.Worktrees.Root, repoRoot))
		}
		return nil
	}
	wtID := row.ID
	slugVal := row.Slug

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_teardown_start",
		"daemon-detached predelete + db teardown + git remove beginning",
		repoID, wtID, "", 0, nil)

	if len(cfg.Hooks.Predelete) > 0 {
		// Same await rationale as postcreate above: predelete teardown
		// (drop external resources, kill watchers) must finish before
		// `TeardownDatabases` blows away the SQL state they may still
		// be using.
		_, _ = hooks.RunHooks(ctx, "predelete", cfg.Hooks.Predelete,
			repoRoot, wtRoot, slugVal, inheritedEnv, true)
	}

	if err := prepare.TeardownDatabases(ctx, &cfg, slugVal, repoID, wtID, st.Store); err != nil {
		return err
	}

	gitArgs := []string{"-C", repoRoot, "worktree", "remove"}
	if force {
		gitArgs = append(gitArgs, "--force")
	}
	gitArgs = append(gitArgs, wtRoot)
	cmd := lowPriorityCommand(ctx, "git", gitArgs)
	if err := cmd.Run(); err != nil && !force {
		return fmt.Errorf("git worktree remove: %w (pass --force to override)", err)
	}

	_ = st.Store.MarkWorktreeDeleted(ctx, wtID)
	pruneEmptyParentsBelow(wtRoot, worktreesRootOf(cfg.Worktrees.Root, repoRoot))
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_teardown_done",
		"daemon-detached teardown complete",
		repoID, wtID, "", 0, nil)
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
// child runs at low CPU + I/O priority. Used for `git worktree
// remove` because `--force` rm -rf's vendor/ + node_modules/ — on a
// Laravel checkout that's ~250k inode unlinks back-to-back. Without
// nice/ionice that storm starves foreground apps for I/O and CPU
// long enough to look like a system freeze.
//
// Layering: `ionice -c 3 nice -n 19 <cmd>`. `ionice -c 3` is the
// idle class — the child only gets disk bandwidth when nothing else
// wants it. `nice -n 19` is the lowest CPU priority. Both are
// best-effort: when either binary is missing we fall back to the
// next layer, and finally to plain exec.Command. macOS shells
// usually carry `nice` but not `ionice`, so the nice-only fallback
// still gives partial relief.
func lowPriorityCommand(ctx context.Context, name string, args []string) *exec.Cmd {
	if _, err := exec.LookPath("ionice"); err == nil {
		ioArgs := append([]string{"-c", "3"}, "nice")
		ioArgs = append(ioArgs, "-n", "19", name)
		ioArgs = append(ioArgs, args...)
		return exec.CommandContext(ctx, "ionice", ioArgs...)
	}
	if _, err := exec.LookPath("nice"); err == nil {
		niceArgs := append([]string{"-n", "19", name}, args...)
		return exec.CommandContext(ctx, "nice", niceArgs...)
	}
	return exec.CommandContext(ctx, name, args...)
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
