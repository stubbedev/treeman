package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/gitx"
	"github.com/stubbedev/treeman/internal/tui"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/wt"
)

// tagRe matches a semver-ish tag (`4.3.21`, `4.3.21-rc1`) → branch
// wizard routes it to `release/<tag>`.
var tagRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+].*)?$`)

// allBranches returns local + remote-only branch names (origin/ prefix
// stripped, origin/HEAD dropped), unique and sorted. One git fork —
// this is on the interactive completion path (fires per <TAB>), so the
// two-fork local+remote split isn't worth it.
func allBranches(dir string) []string {
	out, err := gitcmd.Output(context.Background(), dir,
		"for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/remotes/origin")
	if err != nil {
		return nil
	}
	set := map[string]struct{}{}
	var names []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		b := strings.TrimPrefix(strings.TrimSpace(line), "origin/")
		if b == "" || b == "HEAD" {
			continue
		}
		if _, dup := set[b]; dup {
			continue
		}
		set[b] = struct{}{}
		names = append(names, b)
	}
	sort.Strings(names)
	return names
}

// pickBranch shows a branch picker. Returns the selected branch, the
// typed text when nothing matched (caller decides what a new name
// means), or "" on cancel.
func pickBranch(dir, prompt string) (string, error) {
	branches := allBranches(dir)
	if len(branches) == 0 {
		return "", nil
	}
	res, err := tui.Select(branches, tui.Options{Prompt: prompt})
	if errors.Is(err, tui.ErrCanceled) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if res.Index < 0 {
		return res.Query, nil // typed something not in the list
	}
	return branches[res.Index], nil
}

// defaultBranch resolves the repo default branch (origin/HEAD, else
// master/main).
func defaultBranch(ctx context.Context, repoRoot string) string {
	return wt.DetectDefaultBranch(ctx, repoRoot)
}

// validRef reports whether `name` is a legal branch name.
func validRef(ctx context.Context, repoRoot, name string) bool {
	return gitcmd.RunOptional(ctx, repoRoot, "check-ref-format", "refs/heads/"+name) == nil
}

// switchAction is the shared handler for `git switch` and `wt switch`:
// a branch arg checks it out (worktree-aware, create-or-checkout); no
// arg opens the interactive picker + wizard. Either way goCheckout
// prints the destination path for the shell `cd` shim.
func switchAction(ctx context.Context, c *cli.Command) error {
	repoRoot, err := resolveRepo(c.String("repo"))
	if err != nil {
		return err
	}
	branch := c.Args().First()
	if branch == "" {
		return switchInteractive(ctx, repoRoot, c.String("from"), c.Bool("no-fetch"))
	}
	return goCheckout(ctx, repoRoot, branch, c.String("from"), true, c.Bool("no-fetch"))
}

// switchInteractive drives the no-arg `git switch` / `wt switch` path.
// It shows live worktrees first (newest-commit-first, with * dirty / !
// unpushed markers and their path), then the unoccupied branches; or
// type a new name / Ctrl+C to enter the branch wizard. The selection
// routes through goCheckout, which prints the destination path for the
// shell `cd` shim.
func switchInteractive(ctx context.Context, repoRoot, from string, noFetch bool) error {
	items, targets := buildSwitchMenu(ctx, repoRoot)
	if len(items) == 0 {
		return wizardThenCheckout(ctx, repoRoot, "", noFetch)
	}
	res, err := tui.Select(items, tui.Options{Prompt: "switch/create, Ctrl+C: wizard"})
	switch {
	case errors.Is(err, tui.ErrCanceled), res.Index < 0:
		return wizardThenCheckout(ctx, repoRoot, res.Query, noFetch)
	case err != nil:
		return err
	default:
		return goCheckout(ctx, repoRoot, targets[res.Index], from, true, noFetch)
	}
}

// buildSwitchMenu returns parallel display/target slices for the switch
// picker: `[worktree] <branch><markers>  →  <path>` rows (newest-commit
// first) for live worktrees other than the current one, followed by
// `[branch]   <name>` rows for branches not occupying a worktree. Each
// target is the branch name goCheckout should route on.
func buildSwitchMenu(ctx context.Context, repoRoot string) (items, targets []string) {
	occ := branchOccupancy(ctx, repoRoot)
	cwdTop := ""
	if cwd, err := os.Getwd(); err == nil {
		cwdTop, _ = gitWorktreeRoot(cwd)
	}

	type wtRow struct {
		label, branch string
		ts            int64
	}
	var wrows []wtRow
	for branch, path := range occ {
		if path == cwdTop {
			continue // already here — nothing to switch to
		}
		ts := int64(0)
		if s, err := gitcmd.String(ctx, path, "log", "-1", "--format=%ct", "HEAD"); err == nil {
			ts, _ = strconv.ParseInt(s, 10, 64)
		}
		// Existing worktrees are cyan (live — switching is an instant cd);
		// branch rows below stay default (checkout, maybe create a worktree).
		wrows = append(wrows, wtRow{
			label:  ui.Cyan("[worktree] "+branch+worktreeMarkers(ctx, path)) + "  " + ui.SymArrow + "  " + ui.Dim(path),
			branch: branch, ts: ts,
		})
	}
	sort.Slice(wrows, func(i, j int) bool { return wrows[i].ts > wrows[j].ts })
	for _, r := range wrows {
		items = append(items, r.label)
		targets = append(targets, r.branch)
	}

	for _, b := range allBranches(repoRoot) {
		if _, taken := occ[b]; taken {
			continue
		}
		items = append(items, "[branch]   "+b)
		targets = append(targets, b)
	}
	return items, targets
}

// worktreeMarkers returns " *" (dirty) / " !" (unpushed commits) / " *!"
// suffix for a worktree, or "" when clean and pushed.
func worktreeMarkers(ctx context.Context, path string) string {
	m := ""
	if clean, _ := gitenv.IsWorktreeClean(ctx, path); !clean {
		m += "*"
	}
	if up, _ := gitenv.HasUnpushedCommits(ctx, path); up {
		m += "!"
	}
	if m != "" {
		return " " + m
	}
	return ""
}

// wizardThenCheckout runs the branch wizard and, if it yields a name,
// checks it out. A cancelled wizard is a silent no-op.
func wizardThenCheckout(ctx context.Context, repoRoot, initial string, noFetch bool) error {
	name, base, err := branchWizard(ctx, repoRoot, initial)
	if err != nil || name == "" {
		return err
	}
	return goCheckout(ctx, repoRoot, name, base, true, noFetch)
}

// prefixChoices is the fixed branch-prefix menu.
var prefixChoices = []string{"feature", "bugfix", "hotfix"}

// branchWizard interactively builds a `<prefix>/<name>[-<suffix>]`
// branch (or `release/<tag>` for a semver input) plus its base branch,
// porting the zsh _branch_wizard. Ticket keys and free text are run
// through gitx.Handleize. Returns ("","",nil) on cancel.
func branchWizard(ctx context.Context, repoRoot, initial string) (name, base string, err error) {
	if initial == "" {
		initial, err = tui.Input(tui.InputOptions{Prompt: "branch name [Ticket: ABC-1234 | Tag: 4.3.21]"})
		if errors.Is(err, tui.ErrCanceled) || initial == "" {
			return "", "", nil
		}
		if err != nil {
			return "", "", err
		}
	}

	// Semver tag → release branch off the default branch.
	if tagRe.MatchString(initial) {
		return "release/" + initial, defaultBranch(ctx, repoRoot), nil
	}

	pres, err := tui.Select(prefixChoices, tui.Options{Prompt: "select prefix"})
	if errors.Is(err, tui.ErrCanceled) || pres.Index < 0 {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	prefix := prefixChoices[pres.Index]

	if prefix == "hotfix" {
		base, err = pickBranch(repoRoot, "select base branch for hotfix")
		if err != nil || base == "" {
			return "", "", err
		}
	} else {
		base = defaultBranch(ctx, repoRoot)
	}

	nm := gitx.Handleize(initial)
	suffix, serr := tui.Input(tui.InputOptions{Prompt: "suffix (optional)"})
	if serr != nil && !errors.Is(serr, tui.ErrCanceled) {
		return "", "", serr
	}
	suffix = gitx.Handleize(suffix)

	final := prefix + "/" + nm
	if suffix != "" {
		final += "-" + suffix
	}
	if !validRef(ctx, repoRoot, final) {
		return "", "", fmt.Errorf("invalid branch name: %s", final)
	}
	return final, base, nil
}

// pickWorktree shows a picker over the repo's linked worktrees
// (excluding main) and returns the selected worktree path, or "" on
// cancel / empty list.
func pickWorktree(ctx context.Context, repoRoot, prompt string) (string, error) {
	occ := branchOccupancy(ctx, repoRoot)
	type row struct{ label, path string }
	var rows []row
	for branch, p := range occ {
		if p == repoRoot {
			continue // skip the main worktree
		}
		rows = append(rows, row{branch + worktreeMarkers(ctx, p) + "  " + ui.SymArrow + "  " + p, p})
	}
	if len(rows) == 0 {
		ui.Info("No linked worktrees found.")
		return "", nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].label < rows[j].label })
	items := make([]string, len(rows))
	for i, r := range rows {
		items[i] = r.label
	}
	res, err := tui.Select(items, tui.Options{Prompt: prompt})
	if errors.Is(err, tui.ErrCanceled) || res.Index < 0 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return rows[res.Index].path, nil
}

// worktreeArgComplete emits the branch names of live linked worktrees
// for `<TAB>` completion.
func worktreeArgComplete(_ context.Context, c *cli.Command) {
	if c.NArg() > 0 {
		return
	}
	repoRoot, err := resolveRepo(c.String("repo"))
	if err != nil {
		return
	}
	for branch, p := range branchOccupancy(context.Background(), repoRoot) {
		if p != repoRoot {
			_, _ = fmt.Fprintln(os.Stdout, branch)
		}
	}
}

// branchArgComplete emits branch names for `<TAB>` completion of a
// branch-position argument.
func branchArgComplete(_ context.Context, c *cli.Command) {
	if c.NArg() > 0 {
		return
	}
	dir, err := gitWorkdir(c)
	if err != nil {
		if cwd, cerr := os.Getwd(); cerr == nil {
			dir = cwd
		} else {
			return
		}
	}
	for _, b := range allBranches(dir) {
		_, _ = fmt.Fprintln(os.Stdout, b)
	}
}
