package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// EnrollMainWorktree synchronises the SQLite main-wt row for repoPath
// with the current `main_worktree.enabled` setting in the repo's
// resolved config:
//
//   - enabled=true  → upsert a worktrees row with is_main=1, current
//     branch, branch-aware slug. ListActiveWorktrees then surfaces
//     it and ResumeWorktreeWatcher attaches HEAD + file watchers the
//     same way it does for any linked wt.
//   - enabled=false → if a live main-wt row exists, mark it deleted
//     so the next watcher reload tears its watchers down. Per-branch
//     databases are NOT dropped here — the user opts into that
//     destructive cleanup via `treeman main disable --purge`.
//
// Returns the row id (0 when disabled). A no-op when the config can't
// load (logged by caller) — we never block the boot path on a malformed
// repo config.
func EnrollMainWorktree(ctx context.Context, st *State, repoPath string) (int64, error) {
	// Fast path: a repo with no `.treeman.yaml` can't opt in, so
	// avoid the full resolve+stat hot path on every boot. Treeman
	// instances tracking many repos (50+) where almost none use main
	// worktrees pay otherwise.
	if _, err := os.Stat(filepath.Join(repoPath, ".treeman.yaml")); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
	}

	cfg, err := resolve.LoadResolved(repoPath)
	if err != nil {
		return 0, fmt.Errorf("load cfg: %w", err)
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return 0, fmt.Errorf("ensure repo: %w", err)
	}

	if !cfg.MainWorktree.Enabled {
		existing, err := st.Store.LookupMainWorktree(ctx, repoID)
		if err != nil {
			return 0, fmt.Errorf("lookup existing main: %w", err)
		}
		if existing.ID == 0 {
			return 0, nil
		}
		// Soft-delete only — the row's DBs / hook history stay so a
		// re-enable picks them up. Hard purge is a separate explicit
		// `treeman main disable --purge` flow.
		if err := st.Store.MarkWorktreeDeleted(ctx, existing.ID); err != nil {
			return 0, fmt.Errorf("mark main deleted: %w", err)
		}
		// Preempt any in-flight FinalizeWorktree against the repo root
		// before we tear the row down. Without this a finalize that's
		// halfway through prepare can still write per-branch DBs and
		// emit events keyed off the (about-to-be-deleted) main row,
		// leaving zombie state for the next re-enable to trip over.
		st.CancelFinalize(repoPath)
		// Stop the per-wt watcher we may have spawned for the repo
		// root last time main was enabled. The config reloader's
		// subsequent ListActiveWorktrees skips deleted rows, so it
		// can't unregister this one for us — leaving the goroutine
		// alive would tail HEAD against a path that no longer routes
		// to a row.
		st.UnregisterWtWatcher(repoPath)
		_ = st.Store.WriteEvent(ctx, store.LevelInfo, "main_worktree_disabled",
			"main_worktree.enabled flipped off; row marked deleted",
			repoID, existing.ID, "", 0, map[string]string{"repo": repoPath})
		return 0, nil
	}

	branch := detectBranch(repoPath)
	sl := slug.ForMain(repoPath, branch)
	id, err := st.Store.EnsureMainWorktree(ctx, repoID, repoPath, sl.Value, branch)
	if err != nil {
		return 0, fmt.Errorf("ensure main wt: %w", err)
	}
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "main_worktree_enrolled",
		"main worktree row upserted",
		repoID, id, "", 0, map[string]string{
			"repo":   repoPath,
			"slug":   sl.Value,
			"branch": branch,
		})
	return id, nil
}
