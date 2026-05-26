package wt

import (
	"context"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// Identity bundles the per-event identity of a worktree: the slug
// that names its resources, the upserted store row id, the current
// branch, and whether the path is the repo's main worktree.
type Identity struct {
	Slug   slug.Slug
	WtID   int64
	Branch string
	IsMain bool
}

// ResolveIdentity is the single source of truth for "is this wtPath
// the repo's main worktree?" and the slug/overlay/row-upsert that
// follow. FinalizeWorktree (daemon), the HEAD/FS dispatchers
// (daemon), watcher-driven re-prepare (daemon), CLI-direct hook
// fires (cmd), MCP-driven hook fires (mcp), and the wt-create
// local-finalize child (wt) all route through this so they observe
// the same identity — pre-fix the helper lived in daemon and the
// non-daemon paths drifted, corrupting the main row's slug column
// and rendering hook env vars from the wrong template.
//
// Two-source-of-truth for isMain:
//
//  1. cfg.MainWorktree.Enabled — the live config opts in.
//  2. An active is_main=1 row — bridges the disable→reload race so
//     in-flight events keep using slug.ForMain against the same row.
//
// cfg is mutated in place when the overlay applies; ApplyMainWorktree
// Overlay clones the Databases backing array so resolve.cache readers
// stay isolated. Callers pass `branch` because every package has its
// own .git/HEAD reader (daemon's detectBranch, cmd's
// detectBranchOfWorktree, mcp's detectBranch) — passing it in keeps
// this helper free of the file-stat plumbing.
func ResolveIdentity(
	ctx context.Context,
	st *store.Store,
	cfg *config.Config,
	repoPath, wtPath, branch string,
	repoID int64,
) (Identity, error) {
	isMain := false
	if wtPath == repoPath {
		if cfg.MainWorktree.Enabled {
			isMain = true
		} else if existing, lookupErr := st.LookupMainWorktree(ctx, repoID); lookupErr == nil && existing.ID != 0 {
			isMain = true
		}
	}
	if isMain {
		sl := slug.ForMain(wtPath, branch)
		config.ApplyMainWorktreeOverlay(cfg)
		wtID, err := st.EnsureMainWorktree(ctx, repoID, wtPath, sl.Value, branch)
		return Identity{Slug: sl, WtID: wtID, Branch: branch, IsMain: true}, err
	}
	sl := slug.For(wtPath, branch)
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, branch)
	return Identity{Slug: sl, WtID: wtID, Branch: branch, IsMain: false}, err
}
