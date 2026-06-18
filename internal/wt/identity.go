package wt

import (
	"context"
	"fmt"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitenv"
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
	// Guard against registering a path that is not actually a worktree.
	// Every legitimate caller passes either the repo root (the main
	// worktree) or an existing git-linked worktree directory. A mistyped
	// MCP `worktree` argument that resolved to <cwd>/<typo> would
	// otherwise create a phantom registry row plus per-branch databases
	// that no teardown ever reclaims.
	if wtPath != repoPath && !gitenv.IsLinkedWorktree(wtPath) {
		return Identity{}, fmt.Errorf("refusing to register %q: not a linked worktree", wtPath)
	}
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
	// A linked worktree's slug is keyed off its PATH, not its current
	// branch: the directory is fixed for the worktree's lifetime, so
	// `slug.For(wtPath, "")` is stable across an in-worktree
	// `git checkout`. This keeps a worktree's DB names / patched `.env`
	// / ports fixed while the branch swaps underneath it — the
	// foundation `branch_scoped` swapping relies on. For the common
	// case (the worktree directory was named after its branch at
	// `wt create`) this is identical to `slug.For(wtPath, branch)`.
	sl := slug.For(wtPath, "")
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, branch)
	return Identity{Slug: sl, WtID: wtID, Branch: branch, IsMain: false}, err
}
