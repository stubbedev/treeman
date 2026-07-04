package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

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
		b := strings.TrimSpace(line)
		// `refs/remotes/origin/HEAD` short-forms to bare "origin" — the
		// default-branch symref, not a real branch. Drop it (and its
		// long form) before stripping the remote prefix.
		if b == "" || b == "origin" || b == "origin/HEAD" {
			continue
		}
		b = strings.TrimPrefix(b, "origin/")
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

// switchRoute lands a chosen branch somewhere and prints the
// destination path on stdout. Two policies: checkoutRoute
// (`git switch`, may check out in place) and worktreeRoute
// (`worktree switch`, always a worktree).
type switchRoute func(ctx context.Context, repoRoot, branch, from string, noFetch bool) error

// checkoutRoute — the `git switch` policy (was gcb): branch live in a
// worktree → its path; main clean → checkout in main; main dirty →
// new worktree; else checkout in place.
func checkoutRoute(ctx context.Context, repoRoot, branch, from string, noFetch bool) error {
	return goCheckout(ctx, repoRoot, branch, from, true, noFetch)
}

// worktreeRoute — the `worktree switch` policy (was gwt): the branch
// ALWAYS lands in a worktree. Existing worktree → its path; otherwise
// create/attach one via the full wt-create flow (status + ports on
// stderr; stdout stays the bare path for the cd shim). Never checks
// out in the main repo.
func worktreeRoute(ctx context.Context, repoRoot, branch, from string, noFetch bool) error {
	if path, ok := liveWorktreeForBranch(ctx, repoRoot, branch); ok {
		touchVisitedByPath(ctx, path)
		fmt.Println(path)
		return nil
	}
	// Remote-only branch: seed the worktree from its remote tip, not
	// the repo default (create's pre-fetch resolves it to origin/<b>).
	if from == "" && !wt.RefExistsLocal(ctx, repoRoot, branch) && wt.RefExistsRemote(ctx, repoRoot, branch) {
		from = branch
	}
	return goSpawnWorktree(ctx, repoRoot, branch, from, noFetch)
}

// liveWorktreeForBranch finds the worktree holding `branch`, preferring
// the registry but falling back to `git worktree list` — a worktree
// added outside treeman still exists and must win over a create
// attempt (git would refuse anyway: "already used by worktree"). A
// registry row whose directory is gone (manual rm -rf) is ignored so
// switch never hands the shell a dead path; the create flow self-heals
// the stale row.
func liveWorktreeForBranch(ctx context.Context, repoRoot, branch string) (string, bool) {
	if path, ok := registryWorktreeForBranch(ctx, repoRoot, branch); ok {
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	if path, ok := gitWorktrees(ctx, repoRoot)[branch]; ok {
		if _, err := os.Stat(path); err == nil {
			return path, true
		}
	}
	return "", false
}

// occupiedWorktrees merges the registry occupancy map with git's own
// worktree list (union; registry wins on conflicts) and drops entries
// whose directory no longer exists. This is the source for the switch
// menu + delete picker, so foreign worktrees show up and dead registry
// rows never surface a cd-able path.
func occupiedWorktrees(ctx context.Context, repoRoot string) map[string]string {
	occ := gitWorktrees(ctx, repoRoot)
	if occ == nil {
		occ = map[string]string{}
	}
	maps.Copy(occ, branchOccupancy(ctx, repoRoot))
	for branch, path := range occ {
		if _, err := os.Stat(path); err != nil {
			delete(occ, branch)
		}
	}
	return occ
}

// gitWorktrees parses `git worktree list --porcelain` into
// branch → path for the LINKED worktrees (main checkout and detached
// entries are skipped). The git view catches worktrees created outside
// treeman that the registry doesn't know about.
func gitWorktrees(ctx context.Context, repoRoot string) map[string]string {
	out, err := gitcmd.Output(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	worktrees := map[string]string{}
	var path string
	for line := range strings.SplitSeq(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			branch := strings.TrimPrefix(line, "branch refs/heads/")
			if path != "" && !strings.EqualFold(filepath.Clean(path), filepath.Clean(repoRoot)) {
				worktrees[branch] = path
			}
		case line == "":
			path = ""
		}
	}
	return worktrees
}

// switchAction is the `git switch` handler; wtSwitchAction is the
// `worktree switch` handler. Same UX (arg | picker | wizard), different
// landing policy.
func switchAction(ctx context.Context, c *cli.Command) error {
	return runSwitch(ctx, c, checkoutRoute)
}

func wtSwitchAction(ctx context.Context, c *cli.Command) error {
	return runSwitch(ctx, c, worktreeRoute)
}

func runSwitch(ctx context.Context, c *cli.Command, route switchRoute) error {
	repoRoot, err := resolveRepo(c.String("repo"))
	if err != nil {
		return err
	}
	branch := c.Args().First()
	noFetch := c.Bool("no-fetch")
	if branch == "" {
		return switchInteractive(ctx, repoRoot, c.String("from"), noFetch, route)
	}
	// zsh contract: an unknown BARE name goes through the branch wizard
	// (prefix convention + handleize) instead of silently creating a
	// branch literally named "typo". Names carrying a "/" are taken
	// as-is — the user already chose a prefix.
	if !strings.Contains(branch, "/") && !branchKnown(ctx, repoRoot, branch) {
		if !tui.Interactive() {
			return fmt.Errorf(
				"no branch %q — pass a prefixed name (feature/%s) to create it, or run interactively for the wizard",
				branch, branch)
		}
		return wizardThenRoute(ctx, repoRoot, branch, noFetch, route)
	}
	return route(ctx, repoRoot, branch, c.String("from"), noFetch)
}

// branchKnown reports whether `branch` resolves to anything switchable:
// a local ref, a remote ref, or a live registered worktree.
func branchKnown(ctx context.Context, repoRoot, branch string) bool {
	if wt.RefExistsLocal(ctx, repoRoot, branch) || wt.RefExistsRemote(ctx, repoRoot, branch) {
		return true
	}
	_, ok := registryWorktreeForBranch(ctx, repoRoot, branch)
	return ok
}

// switchInteractive drives the no-arg switch path. It shows live
// worktrees first (newest-commit-first, with * dirty / ! unpushed
// markers and their path), then the unoccupied branches; or type a new
// name / Ctrl+C to enter the branch wizard. The selection lands via
// `route`, which prints the destination path for the shell `cd` shim.
func switchInteractive(ctx context.Context, repoRoot, from string, noFetch bool, route switchRoute) error {
	items, targets := buildSwitchMenu(ctx, repoRoot)
	if len(items) == 0 {
		return wizardThenRoute(ctx, repoRoot, "", noFetch, route)
	}
	res, err := tui.Select(items, tui.Options{Prompt: "switch/create", CancelHint: "wizard"})
	switch {
	case errors.Is(err, tui.ErrAborted):
		return nil // Esc — quit the whole command, no wizard
	case errors.Is(err, tui.ErrCanceled), err == nil && res.Index < 0:
		return wizardThenRoute(ctx, repoRoot, res.Query, noFetch, route)
	case err != nil:
		return err
	default:
		return route(ctx, repoRoot, targets[res.Index], from, noFetch)
	}
}

// buildSwitchMenu returns parallel display/target slices for the switch
// picker: `[worktree] <branch><markers>  →  <path>` rows (newest-commit
// first) for live worktrees other than the current one, followed by
// `[branch]   <name>` rows for branches not occupying a worktree. Each
// target is the branch name goCheckout should route on.
func buildSwitchMenu(ctx context.Context, repoRoot string) (items, targets []string) {
	occ := occupiedWorktrees(ctx, repoRoot)
	cwdTop := ""
	if cwd, err := os.Getwd(); err == nil {
		cwdTop, _ = gitWorktreeRoot(cwd)
	}
	// The branch we're already on has nothing to switch to.
	curBranch := currentBranch(ctx, cmp.Or(cwdTop, repoRoot))

	type wtRow struct {
		label, branch string
		ts            int64
	}
	// Per-worktree state (HEAD timestamp + dirty/unpushed markers) costs
	// ~3 git forks each, and `git status` on a big tree is the slow one.
	// Scan all worktrees CONCURRENTLY — the menu opens after the slowest
	// single worktree instead of the sum (this was the 12s `gwt`).
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		wrows []wtRow
	)
	for branch, path := range occ {
		if path == cwdTop || branch == curBranch {
			continue // already here — nothing to switch to
		}
		wg.Add(1)
		go func(branch, path string) {
			defer wg.Done()
			ts := int64(0)
			if s, err := gitcmd.String(ctx, path, "log", "-1", "--format=%ct", "HEAD"); err == nil {
				ts, _ = strconv.ParseInt(s, 10, 64)
			}
			// Existing worktrees are cyan (live — switching is an instant
			// cd); branch rows below stay default (checkout/create).
			row := wtRow{
				label:  ui.Cyan("[worktree] "+branch+worktreeMarkers(ctx, path)) + "  " + ui.SymArrow + "  " + ui.Dim(path),
				branch: branch, ts: ts,
			}
			mu.Lock()
			wrows = append(wrows, row)
			mu.Unlock()
		}(branch, path)
	}
	wg.Wait()
	sort.Slice(wrows, func(i, j int) bool { return wrows[i].ts > wrows[j].ts })
	for _, r := range wrows {
		items = append(items, r.label)
		targets = append(targets, r.branch)
	}

	for _, b := range allBranches(repoRoot) {
		if _, taken := occ[b]; taken || b == curBranch {
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

// wizardThenRoute runs the branch wizard and, if it yields a name,
// lands it via `route`. A cancelled wizard is a silent no-op.
func wizardThenRoute(ctx context.Context, repoRoot, initial string, noFetch bool, route switchRoute) error {
	name, base, err := branchWizard(ctx, repoRoot, initial)
	if err != nil || name == "" {
		return err
	}
	return route(ctx, repoRoot, name, base, noFetch)
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
		if errors.Is(err, tui.ErrCanceled) {
			return "", "", nil
		}
		// Real failures (e.g. no TTY) must surface — checking the empty
		// string first used to swallow them into a silent no-op exit 0.
		if err != nil {
			return "", "", err
		}
		if initial == "" {
			return "", "", nil
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
	suffix, serr := tui.Input(tui.InputOptions{Prompt: "suffix (optional, ctrl+c skips)"})
	switch {
	case errors.Is(serr, tui.ErrAborted):
		return "", "", nil // Esc — abort the whole wizard
	case serr != nil && !errors.Is(serr, tui.ErrCanceled):
		return "", "", serr
	}
	// Ctrl+C = no suffix; carry on.
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
	occ := occupiedWorktrees(ctx, repoRoot)
	cwdTop := ""
	if cwd, err := os.Getwd(); err == nil {
		cwdTop, _ = gitWorktreeRoot(cwd)
	}
	type row struct{ label, path string }
	// Markers cost a `git status` per worktree — scan concurrently,
	// same as buildSwitchMenu.
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		rows []row
	)
	for branch, p := range occ {
		if p == repoRoot || p == cwdTop {
			continue // skip the main worktree and the one we're standing in
		}
		wg.Add(1)
		go func(branch, p string) {
			defer wg.Done()
			r := row{branch + worktreeMarkers(ctx, p) + "  " + ui.SymArrow + "  " + p, p}
			mu.Lock()
			rows = append(rows, r)
			mu.Unlock()
		}(branch, p)
	}
	wg.Wait()
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
