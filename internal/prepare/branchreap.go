package prepare

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// ReapBranchDurables drops every branch_scoped durable copy belonging to
// `branch` across all of the repo's worktrees. The daemon calls it right
// after it prunes a now-deleted local branch (upstream gone + provably
// merged), so a branch's preserved per-branch databases don't outlive the
// branch.
//
// Because the branch name is known at call time, the durable name is computed
// FORWARD via durable(active, branch) — no reverse lookup of the one-way hash,
// no bookkeeping table. Each (worktree, branch_scoped db) pair yields one
// candidate durable; the existence gate means worktrees that never held the
// branch are silently skipped.
//
// Only the durable copy is ever touched, never the active namespace a live
// worktree connects to. So a mis-rendered active namespace can at worst leave
// a durable un-reaped (a leak) — it can never drop live data.
func ReapBranchDurables(ctx context.Context, cfg *config.Config, st *store.Store, repoID int64, branch string) {
	if branch == "" {
		return
	}
	worktrees, err := st.ListWorktreesForRepo(ctx, repoID)
	if err != nil {
		slog.Warn("reap durables: list worktrees", "repo_id", repoID, "err", err)
		return
	}
	if len(worktrees) == 0 {
		return
	}

	// The main worktree renders its active namespace from the main_worktree
	// overlay (bare, slug-free names); linked worktrees use the base
	// templates. Build the overlaid view once on a shallow copy so the
	// caller's cfg.Databases is left untouched (the overlay reslices into a
	// fresh backing array).
	mainCfg := *cfg
	config.ApplyMainWorktreeOverlay(&mainCfg)

	for i, d := range cfg.Databases {
		if !d.BranchScoped {
			continue
		}
		scope, _, ok := branchScopeFor(d.Engine)
		if !ok {
			continue
		}
		// Reap only touches hash-derived durable namespaces, which never
		// collide with a sibling's prefix — no sibling filter needed.
		eng, closeEng, err := connectBranchEngine(ctx, cfg, d.Engine, nil)
		if err != nil {
			slog.Warn("reap durables: connect engine", "engine", d.Engine, "err", err)
			closeEng()
			continue
		}
		if eng == nil {
			closeEng()
			continue
		}
		for _, wt := range worktrees {
			dbForWt := d
			if wt.IsMain && i < len(mainCfg.Databases) {
				dbForWt = mainCfg.Databases[i]
			}
			active, err := activeNamespace(dbForWt, scope, wt.Path)
			if err != nil {
				continue
			}
			dur := eng.durable(active, branch)
			exists, err := eng.drv.Exists(ctx, dur)
			if err != nil {
				slog.Warn("reap durables: probe", "engine", eng.engine, "durable", dur, "err", err)
				continue
			}
			if !exists {
				continue
			}
			if err := eng.drv.DropDurable(ctx, dur); err != nil {
				slog.Warn("reap durables: drop", "engine", eng.engine,
					"durable", dur, "branch", branch, "err", err)
				continue
			}
			_ = st.WriteEvent(ctx, store.LevelInfo, "branch_durable_reaped",
				fmt.Sprintf("%s: dropped durable for deleted branch %q (active=%s)", eng.engine, branch, active),
				repoID, wt.ID, "", 0, map[string]string{
					"engine":  eng.engine,
					"branch":  branch,
					"durable": dur,
					"active":  active,
				})
		}
		closeEng()
	}
}
