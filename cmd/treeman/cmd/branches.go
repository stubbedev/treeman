package cmd

import (
	"context"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
)

// BranchesCmd — `treeman branches`. Unified local + remote-only
// branch list with a per-branch occupancy column (which live
// worktree, if any, has it checked out). Lets an fzf picker work
// from one JSON call instead of forking `git for-each-ref` plus a
// per-branch `git worktree list` filter.
func BranchesCmd() *cli.Command {
	return &cli.Command{
		Name:  "branches",
		Usage: "list local + remote-only branches with worktree occupancy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "repo root override"},
			&cli.BoolFlag{Name: "json"},
			&cli.BoolFlag{Name: "local-only", Usage: "only branches with a local ref"},
			&cli.BoolFlag{Name: "remote-only", Usage: "only branches that exist on origin but not locally"},
			&cli.BoolFlag{Name: "available", Usage: "only branches not currently occupying a live worktree"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}

			localSet := make(map[string]struct{})
			for _, b := range listGitBranches(repoRoot, false) {
				localSet[b] = struct{}{}
			}
			remoteSet := make(map[string]struct{})
			for _, b := range listGitBranches(repoRoot, true) {
				remoteSet[b] = struct{}{}
			}

			// Build occupancy map: branch → worktree path.
			occ := branchOccupancy(ctx, repoRoot)

			rows := mergeBranchRows(localSet, remoteSet, occ, branchFilter{
				localOnly:  c.Bool("local-only"),
				remoteOnly: c.Bool("remote-only"),
				available:  c.Bool("available"),
			})

			if c.Bool("json") {
				return jsonStream(rows)
			}
			if len(rows) == 0 {
				ui.Info("no branches match the given filters")
				return nil
			}
			renderBranchTable(rows)
			return nil
		},
	}
}

// branchRow is one merged local+remote branch entry with worktree
// occupancy info.
type branchRow struct {
	Branch      string `json:"branch"`
	Local       bool   `json:"local"`
	Remote      bool   `json:"remote"`
	Worktree    string `json:"worktree,omitempty"`
	HasWorktree bool   `json:"has_worktree"`
}

// branchFilter captures the CLI include/exclude flags for branches.
type branchFilter struct {
	localOnly  bool
	remoteOnly bool
	available  bool
}

// mergeBranchRows folds the local set, remote set and occupancy map
// into a sorted, filtered slice of branchRow.
func mergeBranchRows(localSet, remoteSet map[string]struct{}, occ map[string]string, f branchFilter) []branchRow {
	seen := make(map[string]*branchRow)
	for b := range localSet {
		r := &branchRow{Branch: b, Local: true}
		_, r.Remote = remoteSet[b]
		if p, ok := occ[b]; ok {
			r.Worktree = p
			r.HasWorktree = true
		}
		seen[b] = r
	}
	for b := range remoteSet {
		if _, ok := seen[b]; ok {
			continue
		}
		seen[b] = &branchRow{Branch: b, Remote: true}
	}

	var rows []branchRow
	for _, r := range seen {
		if f.localOnly && !r.Local {
			continue
		}
		if f.remoteOnly && (r.Local || !r.Remote) {
			continue
		}
		if f.available && r.HasWorktree {
			continue
		}
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Branch < rows[j].Branch })
	return rows
}

// renderBranchTable prints the human-readable branch table.
func renderBranchTable(rows []branchRow) {
	tbl := ui.NewTable("BRANCH", "ORIGIN", "WORKTREE")
	for _, r := range rows {
		origin := "-"
		switch {
		case r.Local && r.Remote:
			origin = "local+remote"
		case r.Local:
			origin = "local"
		case r.Remote:
			origin = ui.Dim("remote-only")
		}
		wt := ui.Dim("-")
		if r.Worktree != "" {
			wt = ui.Cyan(r.Worktree)
		}
		tbl.Row(r.Branch, origin, wt)
	}
	tbl.Render(nil)
}

// listGitBranches returns local branch names (or origin-remote
// branches with the `origin/` prefix stripped + HEAD pseudo-ref
// dropped). One git fork per call, driven by `%(refname)` so no
// markers / colors leak into the output.
func listGitBranches(repoRoot string, remote bool) []string {
	var args []string
	if remote {
		args = []string{"branch", "-r", "--format=%(refname:short)"}
	} else {
		args = []string{"branch", "--format=%(refname:short)"}
	}
	out, err := gitcmd.Output(context.Background(), repoRoot, args...)
	if err != nil {
		return nil
	}
	var names []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		b := strings.TrimSpace(line)
		if b == "" {
			continue
		}
		if remote {
			if !strings.HasPrefix(b, "origin/") {
				continue
			}
			b = strings.TrimPrefix(b, "origin/")
			if b == "HEAD" {
				continue
			}
		}
		names = append(names, b)
	}
	return names
}

// branchOccupancy returns branch → worktree-path for every live
// worktree of `repoRoot`. One SQLite round-trip — cheaper than a
// per-worktree `git worktree list --porcelain` parse. Worktrees
// mid-teardown are excluded — they feed completion and the pickers,
// and must never surface a path that's about to disappear.
func branchOccupancy(ctx context.Context, repoRoot string) map[string]string {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return nil
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }()
	//nolint:gosec // WorktreeNotTearingDown is a compile-time constant fragment
	rows, err := st.DB.QueryContext(ctx, `
		SELECT COALESCE(w.branch, ''), w.path
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? COLLATE NOCASE AND w.deleted_at IS NULL AND w.branch IS NOT NULL
		AND `+store.WorktreeNotTearingDown, repoRoot)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var branch, path string
		if err := rows.Scan(&branch, &path); err != nil {
			continue
		}
		if branch == "" {
			continue
		}
		out[branch] = path
	}
	return out
}
