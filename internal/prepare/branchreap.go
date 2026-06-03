package prepare

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
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
			_ = st.DeleteBranchDurable(ctx, repoID, dur)
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

// ReapOrphanDurables drops every TRACKED branch_scoped durable whose branch no
// longer exists as a local git ref, and removes its tracking row. It is the
// catch-all that ReapBranchDurables structurally can't be: that reaper
// forward-computes one just-deleted branch's durable name across LIVE
// worktrees, so it misses durables left by a removed worktree or a branch
// deleted out-of-band (the source of the 511-orphan-index leak). This sweep
// enumerates the recorded pool instead and drops by stored NAME — no hash
// re-derivation, no dependence on the worktree still existing.
//
// `repoRoot` is the repo's main checkout, used to list local branches. On a
// git error (or a repo with no listable branches) it declines to drop
// anything rather than risk wiping live durables.
func ReapOrphanDurables(ctx context.Context, cfg *config.Config, st *store.Store, repoID int64, repoRoot string) {
	durables, err := st.ListBranchDurables(ctx, repoID)
	if err != nil {
		slog.Warn("reap orphan durables: list", "repo_id", repoID, "err", err)
		return
	}
	if len(durables) == 0 {
		return
	}
	live, ok := localBranchSet(ctx, repoRoot)
	if !ok || len(live) == 0 {
		// Couldn't enumerate branches (transient git error, or a worktree
		// with no refs) — declining to drop is the safe default; the next
		// tick retries once git is readable.
		return
	}

	// Group orphans by engine so each engine connects at most once.
	byEngine := map[string][]store.BranchDurableRow{}
	for _, d := range durables {
		if _, alive := live[d.Branch]; alive {
			continue
		}
		byEngine[d.Engine] = append(byEngine[d.Engine], d)
	}
	for eng, rows := range byEngine {
		// Reap only touches hash-derived durable namespaces by exact name —
		// no sibling filter needed.
		be, closeEng, cerr := connectBranchEngine(ctx, cfg, eng, nil)
		if cerr != nil {
			slog.Warn("reap orphan durables: connect engine", "engine", eng, "err", cerr)
			closeEng()
			continue
		}
		if be == nil {
			closeEng()
			continue
		}
		for _, d := range rows {
			if err := be.drv.DropDurable(ctx, d.DurableName); err != nil {
				slog.Warn("reap orphan durables: drop", "engine", eng,
					"durable", d.DurableName, "branch", d.Branch, "err", err)
				continue
			}
			_ = st.DeleteBranchDurable(ctx, repoID, d.DurableName)
			_ = st.WriteEvent(ctx, store.LevelInfo, "branch_durable_reaped",
				fmt.Sprintf("%s: dropped orphan durable for absent branch %q (active=%s)", eng, d.Branch, d.DBKey),
				repoID, d.WorktreeID, "", 0, map[string]string{
					"engine":  eng,
					"branch":  d.Branch,
					"durable": d.DurableName,
					"active":  d.DBKey,
					"reason":  "orphan_branch_gone",
				})
		}
		closeEng()
	}
}

// localBranchSet returns the repo's local branch names as a set. ok=false on a
// git error (not a repo / git missing) so the orphan sweep can decline to act
// rather than treat "couldn't list" as "every branch is gone".
func localBranchSet(ctx context.Context, repoRoot string) (map[string]struct{}, bool) {
	out, err := gitcmd.Output(ctx, repoRoot, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil, false
	}
	set := map[string]struct{}{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if b := strings.TrimSpace(line); b != "" {
			set[b] = struct{}{}
		}
	}
	return set, true
}
