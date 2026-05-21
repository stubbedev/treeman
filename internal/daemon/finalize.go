package daemon

import (
	"context"
	"fmt"
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
		_, err := hooks.RunHooks(ctx, "postcreate", cfg.Hooks.Postcreate,
			repoRoot, wtRoot, sl.Value, inheritedEnv)
		if err != nil {
			return fmt.Errorf("postcreate hooks: %w", err)
		}
	}

	if len(cfg.Databases) > 0 {
		if _, err := prepare.Run(ctx, &cfg, repoRoot, wtRoot, sl, st.Store, repoID, wtID, inheritedEnv); err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
	}

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_finalize_done",
		"daemon-detached postcreate + prepare complete",
		repoID, wtID, "", 0, nil)
	return nil
}

// TeardownWorktree mirrors FinalizeWorktree for `treeman wt delete`.
// Runs predelete hooks + DB teardown + `git worktree remove`. All
// in the daemon's runtime so the CLI returns immediately.
func TeardownWorktree(
	ctx context.Context,
	st *State,
	repoPath, worktreePath string,
	force bool,
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

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_teardown_start",
		"daemon-detached predelete + db teardown + git remove beginning",
		repoID, wtID, "", 0, nil)

	if len(cfg.Hooks.Predelete) > 0 {
		_, _ = hooks.RunHooks(ctx, "predelete", cfg.Hooks.Predelete,
			repoRoot, wtRoot, sl.Value, inheritedEnv)
	}

	if err := prepare.TeardownDatabases(ctx, &cfg, sl.Value, repoID, wtID, st.Store); err != nil {
		return err
	}

	args := []string{"-C", repoRoot, "worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wtRoot)
	cmd := exec.CommandContext(ctx, "git", args...)
	if err := cmd.Run(); err != nil && !force {
		return fmt.Errorf("git worktree remove: %w (pass --force to override)", err)
	}

	_ = st.Store.MarkWorktreeDeleted(ctx, wtID)
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_teardown_done",
		"daemon-detached teardown complete",
		repoID, wtID, "", 0, nil)
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
