package daemon

import (
	"context"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
)

// resolveWorktreeIdentity is the single source of truth for "is this
// wtPath the repo's main worktree?" and the slug / overlay / row
// upsert that follow from the answer. FinalizeWorktree, the HEAD- and
// FS-watcher trigger dispatchers, and the watcher-driven re-prepare
// all route through this so they observe the same identity — without
// it the dispatchers would call slug.For + EnsureWorktree on the
// repo-root path and overwrite the main row's slug column on every
// event.
//
// Two-source-of-truth for isMain matches FinalizeWorktree's original
// logic (see the comment block in FinalizeWorktree for the rationale):
//
//  1. cfg.MainWorktree.Enabled — the live config opts in.
//  2. An active is_main=1 row — bridges the disable→reload race so
//     in-flight events keep using slug.ForMain against the same row.
//
// cfg is mutated in place when the overlay applies; ApplyMainWorktree
// Overlay clones the Databases backing array so resolve.cache readers
// stay isolated.
func resolveWorktreeIdentity(
	ctx context.Context,
	st *State,
	cfg *config.Config,
	repoPath, wtPath string,
	repoID int64,
) (sl slug.Slug, wtID int64, branch string, isMain bool, err error) {
	branch = detectBranch(wtPath)
	if wtPath == repoPath {
		if cfg.MainWorktree.Enabled {
			isMain = true
		} else if existing, lookupErr := st.Store.LookupMainWorktree(ctx, repoID); lookupErr == nil && existing.ID != 0 {
			isMain = true
		}
	}
	if isMain {
		sl = slug.ForMain(wtPath, branch)
		config.ApplyMainWorktreeOverlay(cfg)
		wtID, err = st.Store.EnsureMainWorktree(ctx, repoID, wtPath, sl.Value, branch)
		return
	}
	sl = slug.For(wtPath, branch)
	wtID, err = st.Store.EnsureWorktree(ctx, repoID, wtPath, sl.Value, branch)
	return
}
