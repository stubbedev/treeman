package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/gitx"
	"github.com/stubbedev/treeman/internal/tui"
	"github.com/stubbedev/treeman/internal/ui"
)

// GitCmd — `treeman git <verb>`. Ports the git workflow that used to
// live in the dotfiles zsh wrappers (commit with ticket prefix,
// protected-branch push guard, interactive stage/log/stash pickers,
// branch diffs) into the binary with a native TUI. Verbs operate on
// the CURRENT worktree (cwd's toplevel), not the main repo root —
// that's where the user's changes live.
func GitCmd() *cli.Command {
	return &cli.Command{
		Name:  "git",
		Usage: "git workflow helpers (commit, push, add, diff, log, stash, switch)",
		Commands: []*cli.Command{
			gitCommit(),
			gitPush(),
			gitAdd(),
			gitDiff(),
			gitStatus(),
			gitPull(),
			gitFetch(),
			gitStash(),
			gitWipe(),
			gitLog(),
			gitSwitch(),
			gitAmend(),
			gitUndo(),
			gitDiscard(),
			gitBranchDelete(),
			gitSyncBranch(),
			gitFixup(),
		},
	}
}

// gitWorkdir resolves the directory git commands run in: the explicit
// --repo override, else cwd. git resolves the enclosing repo itself and
// these verbs are repo-wide, so we don't pay a `rev-parse --show-toplevel`
// fork here — only gitAdd needs the toplevel (its porcelain paths are
// repo-root-relative) and resolves it directly.
func gitWorkdir(c *cli.Command) (string, error) {
	if r := c.String("repo"); r != "" {
		return MustAbs(r), nil
	}
	return os.Getwd()
}

// repoFlag is the shared --repo/-r flag for git verbs.
func repoFlag() cli.Flag {
	return &cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "worktree/repo dir (default: cwd)"}
}

// currentBranch returns the checked-out branch of dir, or "" if
// detached.
func currentBranch(ctx context.Context, dir string) string {
	b, _ := gitcmd.String(ctx, dir, "branch", "--show-current")
	return b
}

// std wires a git subprocess to the terminal.
func runGit(ctx context.Context, dir string, args ...string) error {
	return gitcmd.RunInteractive(ctx, dir, os.Stdin, os.Stdout, os.Stderr, args...)
}

// gitCommit — `treeman git commit` (was gcm). Prompts for a message,
// auto-prefixes the branch ticket key, and opens $EDITOR when the
// message ends with a trailing backslash.
func gitCommit() *cli.Command {
	return &cli.Command{
		Name:      "commit",
		Usage:     "commit with auto ticket prefix (trailing \\ opens editor)",
		ArgsUsage: "[initial message words...]",
		Flags:     []cli.Flag{repoFlag()},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			prefix := gitx.TicketPrefix(currentBranch(ctx, dir))
			label := "commit message"
			if prefix != "" {
				label = "commit message [" + prefix + "]"
			}
			msg, err := tui.Input(tui.InputOptions{Prompt: label, Initial: strings.Join(c.Args().Slice(), " ")})
			if errors.Is(err, tui.ErrCanceled) {
				return nil
			}
			if err != nil {
				return err
			}

			editor := strings.HasSuffix(msg, `\`)
			if editor {
				msg = strings.TrimRight(strings.TrimSuffix(msg, `\`), " ")
			}
			if msg == "" && !editor {
				return nil // empty message, no editor → abort quietly
			}
			final := gitx.ApplyTicketPrefix(msg, prefix)

			args := []string{"commit"}
			if editor {
				args = append(args, "-e")
			}
			// `git commit -e -m ""` seeds a blank first line; drop -m when
			// there's nothing to seed and we're opening the editor.
			if !editor || final != "" {
				args = append(args, "-m", final)
			}
			return runGit(ctx, dir, args...)
		},
	}
}

// gitPush — `treeman git push` (was gp). Warns before pushing a
// protected branch or when upstream is ahead.
func gitPush() *cli.Command {
	return &cli.Command{
		Name:  "push",
		Usage: "push -u origin <branch> (guards protected branches + divergence)",
		Flags: []cli.Flag{repoFlag()},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			branch := currentBranch(ctx, dir)
			if branch == "" {
				return errors.New("detached HEAD; nothing to push")
			}
			if reason := gitx.ProtectedPush(branch, gpProtectedGlobs(ctx, dir)); reason != "" {
				if !ui.Confirm("Push: " + reason + " — continue?") {
					return nil
				}
			}
			// Divergence: upstream ahead → push will be rejected without --force.
			if n, _ := gitcmd.String(ctx, dir, "rev-list", "--count", "HEAD..@{u}"); n != "" && n != "0" {
				if !ui.Confirm(fmt.Sprintf("Push: upstream is ahead by %s commit(s) — continue?", n)) {
					return nil
				}
			}
			return runGit(ctx, dir, "push", "-u", "origin", branch)
		},
	}
}

// gpProtectedGlobs reads repo-local `gp.protected` globs (repeatable
// via `git config --add gp.protected '<glob>'`).
func gpProtectedGlobs(ctx context.Context, dir string) []string {
	out, err := gitcmd.String(ctx, dir, "config", "--get-all", "gp.protected")
	if err != nil || out == "" {
		return nil
	}
	var globs []string
	for l := range strings.SplitSeq(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			globs = append(globs, l)
		}
	}
	return globs
}

// gitAdd — `treeman git add` (was ga). Interactive multi-select stage:
// modified → `add -p`, deleted/untracked → `add`.
func gitAdd() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "interactively stage files (M→patch, D→removal, A→untracked)",
		ArgsUsage: "[paths to pre-stage...]",
		Flags:     []cli.Flag{repoFlag()},
		Action: func(ctx context.Context, c *cli.Command) error {
			base, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			// Stage from the worktree root: `git status --porcelain` paths
			// are repo-root-relative, so `git add` must run there to resolve
			// them (a subdir cwd would misinterpret them).
			dir, err := gitWorktreeRoot(base)
			if err != nil {
				return err
			}
			if c.NArg() > 0 { // pre-stage explicit paths, like the zsh `ga <path>`
				if err := runGit(ctx, dir, append([]string{"add"}, c.Args().Slice()...)...); err != nil {
					return err
				}
			}
			porcelain, _ := gitcmd.String(ctx, dir, "status", "--porcelain")
			rows := expandStageRows(dir, gitx.StageRows(porcelain))
			if len(rows) == 0 {
				ui.Info("Nothing to stage.")
				return nil
			}
			items := make([]string, len(rows))
			for i, r := range rows {
				items[i] = r.Kind.String() + "  " + r.Path
			}
			res, err := tui.MultiSelect(items, tui.Options{
				Prompt: "stage",
				Header: "[M]odified → patch  [D]eleted → removal  [A]dded → untracked",
			})
			if errors.Is(err, tui.ErrCanceled) {
				return nil
			}
			if err != nil {
				return err
			}
			for _, idx := range res.Indices {
				r := rows[idx]
				switch r.Kind {
				case gitx.StageModified:
					if err := runGit(ctx, dir, "add", "-p", r.Path); err != nil {
						return err
					}
				default: // StageDeleted, StageAdded
					if err := runGit(ctx, dir, "add", r.Path); err != nil {
						return err
					}
				}
			}
			return runGit(ctx, dir, "status", "--short")
		},
	}
}

// expandStageRows walks untracked directories into their contained
// files so each is an individually-stageable row (zsh globbed
// `${dir}**/*`).
func expandStageRows(dir string, rows []gitx.StageRow) []gitx.StageRow {
	var out []gitx.StageRow
	for _, r := range rows {
		if !r.ExpandDir {
			out = append(out, r)
			continue
		}
		root := filepath.Join(dir, r.Path)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d == nil || d.IsDir() {
				return nil //nolint:nilerr // skip unreadable entries, keep walking
			}
			if rel, err := filepath.Rel(dir, p); err == nil {
				out = append(out, gitx.StageRow{Kind: gitx.StageAdded, Path: rel})
			}
			return nil
		})
	}
	return out
}

// gitDiff — `treeman git diff` (was gsd/gd/gcd). No arg → working-tree
// diff. Branch arg (or --pick) → three-dot diff since fork point.
// --patch writes the diff to `<cur>--<target>.diff` instead of paging.
func gitDiff() *cli.Command {
	return &cli.Command{
		Name:      "diff",
		Usage:     "working-tree diff, or three-dot diff vs a branch (--pick, --patch)",
		ArgsUsage: "[branch]",
		Flags: []cli.Flag{
			repoFlag(),
			&cli.BoolFlag{Name: "pick", Usage: "pick the comparison branch interactively"},
			&cli.BoolFlag{Name: "patch", Usage: "write the diff to <cur>--<target>.diff instead of paging"},
		},
		ShellComplete: branchArgComplete,
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			target := c.Args().First()
			if target == "" && c.Bool("pick") {
				target, err = pickBranch(dir, "diff against")
				if err != nil {
					return err
				}
				if target == "" {
					return nil
				}
			}
			if target == "" {
				if c.Bool("patch") {
					return errors.New("--patch needs a comparison branch (pass one or use --pick)")
				}
				// plain working-tree diff; git pages itself
				return runGit(ctx, dir, "diff")
			}
			// A typed-new picker entry (or a typo) isn't a ref — catch it
			// here instead of surfacing git's raw revision error. Prefer
			// the local ref; fall back to origin/<target> (remote-only).
			if !gitcmd.Exists(ctx, dir, target) {
				if !gitcmd.Exists(ctx, dir, "origin/"+target) {
					return fmt.Errorf("no branch %q", target)
				}
				target = "origin/" + target
			}
			cur := currentBranch(ctx, dir)
			spec := cur + "..." + target
			if c.Bool("patch") {
				out, err := gitcmd.Output(ctx, dir, "diff", spec)
				if err != nil {
					return err
				}
				name := gitx.PatchFilename(cur, target)
				if err := os.WriteFile(filepath.Join(dir, name), out, 0o644); err != nil {
					return err
				}
				ui.Success("patch written: %s", name)
				return nil
			}
			return runGit(ctx, dir, "diff", spec)
		},
	}
}

// gitStatus — `treeman git status` (was gst).
func gitStatus() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "short status",
		Flags: []cli.Flag{repoFlag()},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			return runGit(ctx, dir, "status", "--short")
		},
	}
}

// gitPull — `treeman git pull` (was gg / ggo). --all pulls all
// branches; --pick pulls a selected origin branch.
func gitPull() *cli.Command {
	return &cli.Command{
		Name:  "pull",
		Usage: "pull current branch (--all, or --pick an origin branch)",
		Flags: []cli.Flag{
			repoFlag(),
			&cli.BoolFlag{Name: "all", Aliases: []string{"a"}, Usage: "pull all branches"},
			&cli.BoolFlag{Name: "pick", Usage: "pull a selected origin branch"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			if c.Bool("pick") {
				branch, err := pickBranch(dir, "select branch to pull from origin")
				if err != nil || branch == "" {
					return err
				}
				return runGit(ctx, dir, "pull", "origin", branch)
			}
			// Ensure an upstream so a bare `git pull` works on a fresh branch.
			if branch := currentBranch(ctx, dir); branch != "" {
				if gitcmd.RunOptional(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}") != nil {
					_ = gitcmd.Run(ctx, dir, "branch", "--set-upstream-to=origin/"+branch)
				}
			}
			if c.Bool("all") {
				return runGit(ctx, dir, "pull", "--all")
			}
			return runGit(ctx, dir, "pull")
		},
	}
}

// gitFetch — `treeman git fetch` (was gf).
func gitFetch() *cli.Command {
	return &cli.Command{
		Name:  "fetch",
		Usage: "fetch (--all for all remotes)",
		Flags: []cli.Flag{
			repoFlag(),
			&cli.BoolFlag{Name: "all", Aliases: []string{"a"}},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			if c.Bool("all") {
				return runGit(ctx, dir, "fetch", "--all")
			}
			return runGit(ctx, dir, "fetch")
		},
	}
}

// gitStash — `treeman git stash` (was gsa) with `pop` (gsp) and
// `clear` (gcs) subcommands.
func gitStash() *cli.Command {
	return &cli.Command{
		Name:  "stash",
		Usage: "stash all local changes",
		Flags: []cli.Flag{repoFlag()},
		Commands: []*cli.Command{
			{
				Name:  "pop",
				Usage: "pop a selected stash",
				Flags: []cli.Flag{repoFlag()},
				Action: func(ctx context.Context, c *cli.Command) error {
					dir, err := gitWorkdir(c)
					if err != nil {
						return err
					}
					list, _ := gitcmd.String(ctx, dir, "stash", "list")
					if strings.TrimSpace(list) == "" {
						ui.Info("No stashes found.")
						return nil
					}
					lines := strings.Split(list, "\n")
					res, err := tui.Select(lines, tui.Options{Prompt: "pop stash"})
					if errors.Is(err, tui.ErrCanceled) || res.Index < 0 {
						return nil
					}
					if err != nil {
						return err
					}
					ref, _, _ := strings.Cut(lines[res.Index], ":")
					return runGit(ctx, dir, "stash", "pop", ref)
				},
			},
			{
				Name:  "clear",
				Usage: "drop the entire stash stack (with confirmation)",
				Flags: []cli.Flag{repoFlag()},
				Action: func(ctx context.Context, c *cli.Command) error {
					dir, err := gitWorkdir(c)
					if err != nil {
						return err
					}
					list, _ := gitcmd.String(ctx, dir, "stash", "list")
					if strings.TrimSpace(list) == "" {
						ui.Info("Stash is empty.")
						return nil
					}
					n := len(strings.Split(list, "\n"))
					if !ui.Confirm(fmt.Sprintf("Drop all %d stash(es)?", n)) {
						return nil
					}
					return runGit(ctx, dir, "stash", "clear")
				},
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			return runGit(ctx, dir, "stash")
		},
	}
}

// gitWipe — `treeman git wipe` (was gw). Stash + drop local changes.
// --all also clears the entire stash stack.
func gitWipe() *cli.Command {
	return &cli.Command{
		Name:  "wipe",
		Usage: "wipe local changes (stash + drop; --all also clears the stash stack)",
		Flags: []cli.Flag{
			repoFlag(),
			&cli.BoolFlag{Name: "all", Aliases: []string{"a"}},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			if c.Bool("all") {
				if !ui.Confirm("Wipe working changes + entire stash stack?") {
					return nil
				}
				_ = gitcmd.Run(ctx, dir, "stash", "push", "-u")
				return gitcmd.Run(ctx, dir, "stash", "clear")
			}
			if clean, _ := gitenv.IsWorktreeClean(ctx, dir); clean {
				return nil
			}
			if !ui.Confirm("Wipe non-pushed changes on local branch?") {
				return nil
			}
			if err := gitcmd.Run(ctx, dir, "stash", "push", "-u"); err != nil {
				return err
			}
			return gitcmd.Run(ctx, dir, "stash", "drop")
		},
	}
}

// gitLog — `treeman git log` (was gl). Interactive multi-select log;
// Enter shows a commit, Ctrl+X cherry-picks the marked set, Ctrl+R
// reverts them, Ctrl+Y copies the highlighted hash.
func gitLog() *cli.Command {
	return &cli.Command{
		Name:  "log",
		Usage: "interactive log (Enter: show, Ctrl+X: cherry-pick, Ctrl+R: revert, Ctrl+Y: copy hash)",
		Flags: []cli.Flag{
			repoFlag(),
			// The picker holds the whole list in memory (fzf streamed;
			// we don't) — cap it so huge histories stay instant.
			&cli.IntFlag{Name: "limit", Aliases: []string{"n"}, Value: 300, Usage: "number of commits to load"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dir, err := gitWorkdir(c)
			if err != nil {
				return err
			}
			out, err := gitcmd.String(ctx, dir, "log", "--oneline", "--no-color",
				"-n", strconv.Itoa(c.Int("limit")))
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) == "" {
				ui.Info("No commits.")
				return nil
			}
			lines := strings.Split(out, "\n")
			res, err := tui.MultiSelect(lines, tui.Options{
				Prompt: "show",
				Actions: []tui.Action{
					{Key: "ctrl+x", Name: "cherry-pick", Hint: "ctrl+x cherry-pick", NeedsSelection: true},
					{Key: "ctrl+r", Name: "revert", Hint: "ctrl+r revert", NeedsSelection: true},
					{Key: "ctrl+y", Name: "copy", Hint: "ctrl+y copy hash"},
				},
			})
			if errors.Is(err, tui.ErrCanceled) {
				return nil
			}
			if err != nil {
				return err
			}
			hashOf := func(idx int) string {
				h, _, _ := strings.Cut(lines[idx], " ")
				return h
			}
			switch res.Action {
			case "copy":
				if res.Index < 0 {
					return nil
				}
				if err := copyClipboard(hashOf(res.Index)); err != nil {
					ui.Warn("copy failed: %v", err)
					return nil
				}
				ui.Success("copied %s", hashOf(res.Index))
				return nil
			case "cherry-pick", "revert":
				// git log is newest-first; replay oldest-first so history
				// applies forward.
				var hashes []string
				for _, idx := range slices.Backward(res.Indices) {
					hashes = append(hashes, hashOf(idx))
				}
				verb := "cherry-pick"
				if res.Action == "revert" {
					verb = "revert"
				}
				return runGit(ctx, dir, append([]string{verb}, hashes...)...)
			default: // Enter → show the highlighted commit
				if res.Index < 0 {
					return nil
				}
				return runGit(ctx, dir, "show", "--stat", "--patch", hashOf(res.Index))
			}
		},
	}
}

// gitSwitch — `treeman git switch` (was gcb). Checkout-or-create a
// branch with worktree-aware routing (reuses goCheckout). Prints the
// destination path on stdout for the shell `cd` shim; a bare invocation
// with no branch runs the interactive picker + wizard (see wt switch).
func gitSwitch() *cli.Command {
	return &cli.Command{
		Name:      "switch",
		Usage:     "checkout or create a branch, worktree-aware (prints dest path for cd)",
		ArgsUsage: "[branch]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.StringFlag{Name: "from", Usage: "base branch when creating"},
			&cli.BoolFlag{Name: "no-fetch", Usage: "skip the pre-checkout fetch"},
		},
		ShellComplete: branchArgComplete,
		Action:        switchAction,
	}
}

// copyClipboard writes s to the system clipboard via whichever tool is
// on PATH (wl-copy / xclip / pbcopy). No clipboard dependency — the
// zsh wrapper shelled out the same way.
func copyClipboard(s string) error {
	tools := [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"pbcopy"}}
	for _, t := range tools {
		if _, err := exec.LookPath(t[0]); err != nil {
			continue
		}
		cmd := exec.Command(t[0], t[1:]...) //nolint:noctx // fire-and-forget clipboard write
		cmd.Stdin = strings.NewReader(s)
		return cmd.Run()
	}
	return errors.New("no clipboard tool found (install wl-copy, xclip, or pbcopy)")
}
