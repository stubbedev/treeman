package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitx"
	"github.com/stubbedev/treeman/internal/tui"
	"github.com/stubbedev/treeman/internal/ui"
)

// runGitEnv runs a terminal-wired git command with extra env entries.
func runGitEnv(ctx context.Context, dir string, env []string, args ...string) error {
	return gitcmd.RunInteractiveEnv(ctx, dir, env, os.Stdin, os.Stdout, os.Stderr, args...)
}

// gitAmend — `treeman git amend`. Amend the last commit; --no-edit
// folds the staged changes in without reopening the message.
func gitAmend() *cli.Command {
	return &cli.Command{
		Name:  "amend",
		Usage: "amend the last commit (--no-edit keeps its message)",
		Flags: []cli.Flag{
			repoFlag(),
			&cli.BoolFlag{Name: "no-edit", Usage: "keep the existing message; just fold in staged changes"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			args := []string{"commit", "--amend"}
			if c.Bool("no-edit") {
				args = append(args, "--no-edit")
			}
			return runGit(ctx, dir, args...)
		},
	}
}

// gitUndo — `treeman git undo`. Uncommit the last commit, keeping its
// changes staged (`reset --soft HEAD~1`).
func gitUndo() *cli.Command {
	return &cli.Command{
		Name:  "undo",
		Usage: "uncommit the last commit, keeping its changes staged (reset --soft HEAD~1)",
		Flags: []cli.Flag{repoFlag()},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			subject, _ := gitcmd.String(ctx, dir, "log", "-1", "--format=%h %s")
			if err := runGit(ctx, dir, "reset", "--soft", "HEAD~1"); err != nil {
				return err
			}
			if subject != "" {
				ui.Success("undid %s — changes kept staged", subject)
			}
			return nil
		},
	}
}

// gitDiscard — `treeman git discard`. Interactive picker to throw away
// working-tree changes on selected files (per-file complement to
// `git wipe`). Modified/deleted → `git restore`; untracked → delete.
func gitDiscard() *cli.Command {
	return &cli.Command{
		Name:  "discard",
		Usage: "discard working-tree changes on selected files (irreversible)",
		Flags: []cli.Flag{repoFlag()},
		Action: func(ctx context.Context, c *cli.Command) error {
			base, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			dir, err := gitWorktreeRoot(base) // porcelain paths are root-relative
			if err != nil {
				return err
			}
			porcelain, _ := gitcmd.String(ctx, dir, "status", "--porcelain")
			rows := expandStageRows(dir, gitx.StageRows(porcelain))
			if len(rows) == 0 {
				ui.Info("Nothing to discard.")
				return nil
			}
			sortStageRows(rows)
			items := make([]string, len(rows))
			for i, r := range rows {
				items[i] = r.Kind.String() + "  " + r.Path
			}
			res, err := tui.MultiSelect(items, tui.Options{
				Prompt: "discard",
				Header: "[M]odified / [D]eleted → restore   [A]dded → delete untracked",
			})
			if errors.Is(err, tui.ErrCanceled) || len(res.Indices) == 0 {
				return nil
			}
			if err != nil {
				return err
			}
			if !ui.Confirm("Discard changes on " + plural(len(res.Indices), "file", "files") + " (irreversible)?") {
				return nil
			}
			for _, idx := range res.Indices {
				r := rows[idx]
				switch r.Kind {
				case gitx.StageAdded: // untracked → remove from disk
					if err := os.RemoveAll(filepath.Join(dir, r.Path)); err != nil {
						ui.Warn("rm %s: %v", r.Path, err)
					}
				default: // StageModified, StageDeleted → restore from index
					if err := gitcmd.Run(ctx, dir, "restore", "--", r.Path); err != nil {
						ui.Warn("restore %s: %v", r.Path, err)
					}
				}
			}
			return runGit(ctx, dir, "status", "--short")
		},
	}
}

// gitBranchDelete — `treeman git branch-delete`. Multi-select picker to
// delete local branches; branches already merged into HEAD are marked
// so it's obvious which are safe.
func gitBranchDelete() *cli.Command {
	return &cli.Command{
		Name:  "branch-delete",
		Usage: "delete local branches (picker; merged branches marked)",
		Flags: []cli.Flag{repoFlag()},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			current := currentBranch(ctx, dir)
			merged := mergedBranches(ctx, dir)
			var names, items []string
			for _, b := range listGitBranches(dir, false) {
				if b == current {
					continue // can't delete the checked-out branch
				}
				label := b
				if merged[b] {
					label += ui.Dim(" (merged)")
				}
				names = append(names, b)
				items = append(items, label)
			}
			if len(names) == 0 {
				ui.Info("No other local branches to delete.")
				return nil
			}
			res, err := tui.MultiSelect(items, tui.Options{Prompt: "delete branches"})
			if errors.Is(err, tui.ErrCanceled) || len(res.Indices) == 0 {
				return nil
			}
			if err != nil {
				return err
			}
			if !ui.Confirm("Delete " + plural(len(res.Indices), "local branch", "local branches") + "?") {
				return nil
			}
			for _, idx := range res.Indices {
				// -D (force): the user explicitly picked these, and the
				// (merged) marker already flags the unmerged ones.
				if err := gitcmd.Run(ctx, dir, "branch", "-D", names[idx]); err != nil {
					ui.Warn("delete %s: %v", names[idx], err)
				} else {
					ui.Success("deleted %s", names[idx])
				}
			}
			return nil
		},
	}
}

// gitSyncBranch — `treeman git sync-branch`. Bring the current branch
// up to date with its base: fetch, then merge (or --rebase) the base in.
func gitSyncBranch() *cli.Command {
	return &cli.Command{
		Name:  "sync-branch",
		Usage: "update the current branch from its base (fetch + merge/rebase base in)",
		Flags: []cli.Flag{
			repoFlag(),
			&cli.StringFlag{Name: "base", Usage: "base branch (default: repo default branch)"},
			&cli.BoolFlag{Name: "rebase", Usage: "rebase onto the base instead of merging"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			base := c.String("base")
			if base == "" {
				base = defaultBranch(ctx, dir)
			}
			if base == "" {
				return errors.New("could not determine a base branch (pass --base)")
			}
			if err := runGit(ctx, dir, "fetch", "origin", base); err != nil {
				return err
			}
			if c.Bool("rebase") {
				return runGit(ctx, dir, "rebase", "origin/"+base)
			}
			return runGit(ctx, dir, "merge", "origin/"+base)
		},
	}
}

// gitFixup — `treeman git fixup`. Commit the staged changes as a
// `fixup!` of a picked commit, then autosquash-rebase so it folds in
// (unless --no-rebase).
func gitFixup() *cli.Command {
	return &cli.Command{
		Name:  "fixup",
		Usage: "commit staged changes as a fixup! of a picked commit, then autosquash",
		Flags: []cli.Flag{
			repoFlag(),
			&cli.BoolFlag{Name: "no-rebase", Usage: "create the fixup! commit but skip the autosquash rebase"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			out, err := gitcmd.String(ctx, dir, "log", "--oneline", "-n", "50", "--no-color")
			if err != nil || strings.TrimSpace(out) == "" {
				return errors.New("no commits to fix up")
			}
			lines := strings.Split(out, "\n")
			res, err := tui.Select(lines, tui.Options{Prompt: "fixup target"})
			if errors.Is(err, tui.ErrCanceled) || res.Index < 0 {
				return nil
			}
			if err != nil {
				return err
			}
			hash, _, _ := strings.Cut(lines[res.Index], " ")
			if err := runGit(ctx, dir, "commit", "--fixup="+hash); err != nil {
				return err
			}
			if c.Bool("no-rebase") {
				ui.Hint("run `git rebase -i --autosquash %s^` to fold it in", hash)
				return nil
			}
			// Non-interactive autosquash: GIT_SEQUENCE_EDITOR=true accepts
			// the generated todo list as-is (the fixup! line is already
			// marked), GIT_EDITOR=true guards any incidental message edit.
			return runGitEnv(ctx, dir,
				[]string{"GIT_SEQUENCE_EDITOR=true", "GIT_EDITOR=true"},
				"rebase", "-i", "--autosquash", hash+"^")
		},
	}
}

// mergedBranches returns the set of local branches already merged into
// the current HEAD (safe to delete).
func mergedBranches(ctx context.Context, dir string) map[string]bool {
	out, err := gitcmd.String(ctx, dir, "branch", "--merged", "--format=%(refname:short)")
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		if b := strings.TrimSpace(line); b != "" {
			set[b] = true
		}
	}
	return set
}

// plural renders "<n> <noun>" picking the singular or plural noun.
func plural(n int, singular, many string) string {
	noun := many
	if n == 1 {
		noun = singular
	}
	return fmt.Sprintf("%d %s", n, noun)
}
