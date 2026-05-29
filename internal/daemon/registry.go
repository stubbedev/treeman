package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/stubbedev/treeman/internal/store"
)

// removeRepoFromRegistry tears down every live watcher attached to
// `repoPath`, then deletes the repo row and its child rows (worktrees,
// events, snapshots, hook_runs) from SQLite.
//
// External resources (per-worktree databases, on-disk worktree dirs,
// dump caches) are deliberately untouched — `registry remove` is the
// "stop tracking" operation, not the destructive teardown. Callers
// who want everything purged run `treeman wt delete --all` first.
//
// When `force` is false and any worktree row still has `deleted_at IS
// NULL`, the call fails with an explanatory error so a routine
// invocation doesn't silently orphan running services.
func removeRepoFromRegistry(ctx context.Context, st *State, repoPath string, force bool) error {
	if repoPath == "" {
		return errors.New("repo_remove: empty repo_path")
	}
	repoID, err := st.Store.LookupRepoID(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("lookup repo: %w", err)
	}
	if repoID == 0 {
		return fmt.Errorf("repo not enrolled: %s", repoPath)
	}

	if !force {
		n, err := st.Store.CountActiveWorktreesForRepo(ctx, repoID)
		if err != nil {
			return fmt.Errorf("count active worktrees: %w", err)
		}
		if n > 0 {
			return fmt.Errorf("repo has %d active worktree(s); run `treeman wt delete` first or re-run with --force", n)
		}
	}

	// Stop watchers BEFORE deleting rows. A live watcher
	// holds a transaction against the source DB; a wt fsnotify
	// watcher could fire a finalize that would EnsureRepo the row
	// straight back into existence and race the DELETE.
	st.UnregisterWatcher(repoPath)
	st.UnregisterLifecycleWatcher(repoPath)

	wts, err := st.Store.ListActiveWorktrees(ctx)
	if err == nil {
		for _, wt := range wts {
			if wt.RepoPath != repoPath {
				continue
			}
			st.UnregisterWtWatcher(wt.WorktreePath)
		}
	}
	if st.ConfigReloader != nil {
		st.ConfigReloader.RemoveRepo(repoPath)
	}

	if err := st.Store.RemoveRepo(ctx, repoID); err != nil {
		return fmt.Errorf("remove repo: %w", err)
	}
	slog.Info("repo removed from registry", "repo", repoPath, "id", repoID, "force", force)
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "registry_remove",
		"repo removed from registry", 0, 0, "", 0,
		map[string]string{"repo": repoPath, "force": strconv.FormatBool(force)})
	return nil
}
