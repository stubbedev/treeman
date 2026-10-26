package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/tui"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/wt"
	"github.com/stubbedev/treeman/pkg/rpc"
)

// WorktreeCmd — `treeman worktree {create,delete,register,unregister,list,finalize}`.
func WorktreeCmd() *cli.Command {
	return &cli.Command{
		Name:    "worktree",
		Aliases: []string{"wt"},
		Usage:   "worktree lifecycle",
		Action:  worktreeRootAction,
		Commands: []*cli.Command{
			wtCreate(),
			wtDelete(),
			wtRegister(),
			wtUnregister(),
			wtList(),
			wtShow(),
			wtLogs(),
			wtWait(),
			wtFinalize(),
			wtBack(),
			wtPrev(),
			wtGo(),
			wtSwitch(),
			wtPrune(),
			WtGcCmd(),
		},
	}
}

// worktreeRootAction runs when `treeman worktree` gets no subcommand.
// On a terminal that's the management picker over the repo's
// worktrees (Enter = resolve + print path for the cd shim, ctrl+d =
// delete, ctrl+s = show, ctrl+n = create wizard); without a TTY it
// degrades to the subcommand help exactly as before.
func worktreeRootAction(ctx context.Context, c *cli.Command) error {
	if !tui.Interactive() {
		return cli.ShowSubcommandHelp(c)
	}
	repoRoot, err := resolveRepo(c.String("repo"))
	if err != nil {
		// No repo to manage from here — the picker has nothing to list.
		return cli.ShowSubcommandHelp(c)
	}
	return worktreeActionPicker(ctx, c, repoRoot)
}

// worktreeActionPicker is the bare-`treeman worktree` surface: a
// filterable single-select over the repo's worktrees whose plain
// Enter resolves + prints the path (stdout carries only the path —
// the cd-substitution contract), and whose action keys hand the row
// to the same flows as the named subcommands.
func worktreeActionPicker(ctx context.Context, c *cli.Command, repoRoot string) error {
	occ, err := occupiedWorktrees(ctx, repoRoot)
	if err != nil {
		return err
	}
	type row struct{ label, path, branch string }
	// Markers cost a `git status` per worktree — scan concurrently,
	// same as buildSwitchMenu.
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		rows []row
	)
	for branch, p := range occ {
		wg.Add(1)
		go func(branch, p string) {
			defer wg.Done()
			r := row{
				label:  ui.Cyan("[worktree] "+branch+worktreeMarkers(ctx, p)) + "  " + ui.SymArrow + "  " + p,
				path:   p,
				branch: branch,
			}
			mu.Lock()
			rows = append(rows, r)
			mu.Unlock()
		}(branch, p)
	}
	wg.Wait()
	if len(rows) == 0 {
		ui.Info("No worktrees yet.")
		ui.Hint("create one with: treeman worktree create <branch>")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].branch < rows[j].branch })
	items := make([]string, len(rows))
	paths := make([]string, len(rows))
	for i, r := range rows {
		items[i] = r.label
		paths[i] = r.path
	}
	res, err := tui.Select(items, tui.Options{
		Prompt: "manage worktree (enter=open, ctrl+d delete, ctrl+s show, ctrl+n new)",
		Values: paths,
		Actions: []tui.Action{
			{Key: "ctrl+d", Name: "delete", Hint: "delete", NeedsSelection: true},
			{Key: "ctrl+s", Name: "show", Hint: "show", NeedsSelection: true},
			{Key: "ctrl+n", Name: "new", Hint: "new worktree"},
		},
	})
	switch {
	case errors.Is(err, tui.ErrAborted), errors.Is(err, tui.ErrCanceled):
		return nil
	case err != nil:
		return err
	}
	var path string
	if res.Index >= 0 {
		path = paths[res.Index]
	}
	switch res.Action {
	case "delete":
		if path == "" {
			return nil
		}
		// Same flow as `wt delete <path>`: the root command carries no
		// --force/--yes, so the confirmations still apply.
		return deleteWorktreeTarget(ctx, c, repoRoot, path, false)
	case "show":
		if path == "" {
			return nil
		}
		return showWorktree(ctx, showOpts{events: 10, hooks: 5}, path)
	case "new":
		name, base, werr := branchWizard(ctx, repoRoot, res.Query)
		if werr != nil || name == "" {
			return werr
		}
		_, cerr := spawnWorktree(ctx, repoRoot, rpc.WorktreeCreateArgs{Branch: name, From: base}, false)
		return cerr
	default:
		if path == "" {
			return nil
		}
		// cd-shim contract: stdout carries only the resolved path.
		landExisting(ctx, path)
		return nil
	}
}

// wtPrune — `treeman worktree prune`. Runs `git worktree prune` to drop
// stale git admin entries, then reconciles the registry: any live row
// whose worktree directory no longer exists on disk gets a full
// teardown (DB drop + registry row removal). Cleans up after a manual
// `rm -rf` of a worktree.
func wtPrune() *cli.Command {
	return &cli.Command{
		Name:  "prune",
		Usage: "prune worktrees whose directory is gone (git worktree prune + registry reconcile)",
		Flags: []cli.Flag{&cli.StringFlag{Name: "repo", Aliases: []string{"r"}}},
		Action: func(ctx context.Context, c *cli.Command) error {
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			if err := gitcmd.Run(ctx, repoRoot, "worktree", "prune"); err != nil {
				return err
			}
			pruned := 0
			live, err := liveWorktreePaths(ctx, repoRoot)
			if err != nil {
				return err
			}
			for _, p := range live {
				if p == repoRoot {
					continue
				}
				if _, statErr := os.Stat(p); statErr == nil {
					continue // directory still present
				}
				if _, delErr := wt.Delete(ctx, wt.DeleteRequest{
					RepoRoot: repoRoot, Target: p, Force: true, Env: CaptureInheritedEnv(),
				}, cliSink{}); delErr != nil {
					PrintWarn("prune %s: %v", p, delErr)
					continue
				}
				PrintOK("pruned %s (directory gone)", p)
				pruned++
			}
			if pruned == 0 {
				PrintInfo("nothing to prune — all registered worktrees present on disk")
			}
			return nil
		},
	}
}

func liveWorktreePaths(ctx context.Context, repoRoot string) ([]string, error) {
	st, err := openDefaultStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT w.path FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? COLLATE NOCASE AND w.deleted_at IS NULL`, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("list the registered worktrees of %s: %w", repoRoot, err)
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan a registered worktree of %s: %w", repoRoot, err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the registered worktrees of %s: %w", repoRoot, err)
	}
	return paths, nil
}

// wtSwitch — `treeman worktree switch [branch]` (the zsh `gwt`). Lands
// the branch in a worktree — its existing one, else a freshly created
// linked worktree — and NEVER checks out in the main checkout; that
// in-place policy belongs to `git switch` / `go --checkout` (gcb). Same
// UX as `git switch` (arg | picker | wizard, shared flag set), different
// landing policy (worktreeRoute). Prints the destination path on stdout
// for the shell shim: `cd "$(treeman worktree switch …)"`.
func wtSwitch() *cli.Command {
	return &cli.Command{
		Name:          "switch",
		Usage:         "switch to or create a branch's worktree — never checks out in the main repo (prints dest path for cd)",
		ArgsUsage:     "[branch]",
		Flags:         switchFlags(),
		ShellComplete: branchArgComplete,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runSwitch(ctx, c, worktreeRoute)
		},
	}
}

// createWorktree runs a create task to completion and decodes its
// result — the one source of the created worktree's path, so no caller
// has to re-derive it from the branch name.
func createWorktree(ctx context.Context, task rpc.Task) (wt.CreateResult, error) {
	var res wt.CreateResult
	payload, err := resultPayload(ctx, task)
	if err != nil {
		return res, err
	}
	if payload == nil {
		return res, errors.New("worktree create: daemon returned no result")
	}
	if err := json.Unmarshal(payload, &res); err != nil {
		return res, fmt.Errorf("worktree create: decode result: %w", err)
	}
	return res, nil
}

func wtCreate() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Usage:     "create a worktree end-to-end",
		ArgsUsage: "<branch>",
		Description: `Creates a linked worktree, patches the env files, registers it
in SQLite, then dispatches setup hooks + prepare to the daemon.
Default is dispatch-and-return; --foreground subscribes to the run's
events and blocks until finalize completes (its live progress lines
are plain text on stderr, machine-parseable when piped).

Examples:
  treeman worktree create PROJ-1234
  treeman worktree create feature/x --from origin/develop
  treeman worktree create feature/x --foreground   # block until ready
  cd "$(treeman worktree create feat --print-path)"`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "base branch"},
			&cli.StringFlag{Name: "path", Usage: "explicit worktree path"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "repo root override"},
			&cli.BoolFlag{
				Name:  "skip-hooks",
				Usage: "checkout only: no create hooks, no databases (links/copies/patches still applied inline)",
			},
			&cli.BoolFlag{
				Name:  "skip-prepare",
				Usage: "run hooks/links/copies/patches but provision no databases",
			},
			&cli.BoolFlag{
				Name:  "no-fetch",
				Usage: "skip the pre-create `git fetch origin <base>` (defaults on so new branches pick up upstream commits)",
			},
			&cli.BoolFlag{
				Name:  "print-path",
				Usage: "print only the new worktree path on stdout; status lines redirect to stderr (enables `cd \"$(treeman worktree create …)\"`)",
			},
			foregroundFlag(),
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman worktree create <branch>")
			}
			branch := c.Args().First()

			// When --print-path is set the shell idiom is
			// `cd "$(treeman worktree create …)"`; the new path is the
			// LAST line on stdout. Redirect all status output (Print*,
			// ui.Success, etc.) to stderr for the rest of this call
			// so the cd substitution can't pick up an OK marker.
			printPathOnly := c.Bool("print-path")
			if printPathOnly {
				prev := ui.Out
				ui.Out = os.Stderr
				defer func() { ui.Out = prev }()
			}

			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			createArgs := rpc.WorktreeCreateArgs{
				Branch:      branch,
				From:        c.String("from"),
				Path:        c.String("path"),
				NoFetch:     c.Bool("no-fetch"),
				SkipHooks:   c.Bool("skip-hooks"),
				SkipPrepare: c.Bool("skip-prepare"),
			}
			path, err := spawnWorktree(ctx, repoRoot, createArgs, c.Bool("foreground"))
			if err != nil {
				return err
			}
			if printPathOnly {
				_, _ = fmt.Fprintln(os.Stdout, path)
			}
			return nil
		},
	}
}

// spawnWorktree creates the worktree and returns its path. In the
// foreground it streams the daemon's progress and returns only once
// setup hooks + prepare have finished, failing when they failed (the
// checkout is kept); otherwise it reports the queued create and
// returns at once.
func spawnWorktree(ctx context.Context, repoRoot string, args rpc.WorktreeCreateArgs, foreground bool) (string, error) {
	if !foreground {
		res, err := createWorktree(ctx, args.Task(repoRoot, CaptureInheritedEnv()))
		if err != nil {
			return "", err
		}
		printCreateResult(res)
		return res.WtPath, nil
	}
	args.Wait = true
	if err := foregroundTask(ctx, args.Task(repoRoot, CaptureInheritedEnv()), "worktree create"); err != nil {
		return "", err
	}
	path, ok, err := wt.LookupWorktree(ctx, repoRoot, args.Branch, cliSink{})
	if err != nil {
		return "", err
	}
	switch {
	case ok:
	case args.Path != "":
		path = args.Path
	default:
		return "", errors.New("create finished but the worktree row is not visible yet — check `worktree list`")
	}
	ui.Success("ready: %s", path)
	return path, nil
}

// printCreateResult renders the structured worktree_create outcome to
// the status stream (stderr under --print-path). The daemon performed
// the work; this is the CLI's human echo of it.
func printCreateResult(res wt.CreateResult) {
	switch res.Status {
	case wt.CreatedNoop:
		PrintInfo("worktree already exists at %s — no-op", res.WtPath)
		return
	case wt.CreatedNoFinalize:
		PrintOK("created worktree #%d slug=%s path=%s", res.WorktreeID, res.Slug, res.WtPath)
	case wt.CreatedReady:
		PrintOK("created worktree #%d slug=%s path=%s", res.WorktreeID, res.Slug, res.WtPath)
		PrintOK("setup + prepare complete")
	default:
		PrintOK("created worktree #%d slug=%s path=%s", res.WorktreeID, res.Slug, res.WtPath)
		PrintOK("queued: setup + prepare on daemon — `treeman worktree wait %s` blocks until done and fails if setup fails", res.Slug)
	}
	if len(res.Ports) > 0 {
		names := make([]string, 0, len(res.Ports))
		for n := range res.Ports {
			names = append(names, n)
		}
		sort.Strings(names)
		summary := ""
		for _, n := range names {
			if summary != "" {
				summary += " "
			}
			summary += fmt.Sprintf("%s=%d", n, res.Ports[n])
		}
		PrintInfo("ports: %s", summary)
	}
}

func wtDelete() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "delete a worktree end-to-end",
		ArgsUsage: "[path-or-branch...]",
		Description: `Runs teardown hooks + DB teardown + git worktree remove, then
removes the registry row. The teardown is dispatched to the daemon
over the RPC socket — the CLI returns immediately. --foreground
instead blocks until the teardown's worktree:delete:end event lands
(live spinner on a TTY). If the daemon can't be reached (even after
autostart), the teardown runs in-process instead (blocks until done).

A worktree that another process is still using (its working
directory, executable, or an open or mapped file is inside the
worktree) is refused, naming each process, even with --force: deleting
it would pull vendor/, node_modules/ and binaries out from under a
running deploy, server or editor. The invoking shell and treemand's
own processes don't count.

Several targets may be given, and the no-argument picker is a
Tab-toggle multi-select — both delete every named worktree in one
invocation.

Examples:
  treeman worktree delete PROJ-1234
  treeman worktree delete PROJ-1234 --foreground   # block until dropped
  treeman worktree delete PROJ-1234 PROJ-5678       # several at once
  treeman worktree delete /path/to/wt --force      # remove stale registry entry`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "force", Aliases: []string{"f"}},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
			&cli.BoolFlag{
				Name:  "foreground",
				Usage: "block until the daemon's teardown completes (worktree:delete:end); -f stays force, as in git",
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "resolve the target + print the git state and the per-engine drop plan; change nothing, connect nowhere",
			},
			// `--detached` runs the teardown inline in this process
			// instead of dispatching it to the daemon over the socket.
			// Kept as a manual escape hatch (debugging / daemon-less
			// teardown); nothing sets it automatically. Not in help.
			&cli.BoolFlag{Name: "detached", Hidden: true},
		},
		ShellComplete: worktreeArgsComplete,
		Action: func(ctx context.Context, c *cli.Command) error {
			targets := c.Args().Slice()

			// Resolve the repo root first (--repo flag > cwd walk-up >
			// fall back to target-as-path). Needed before the branch /
			// slug lookup so the SQLite query is scoped correctly.
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				if len(targets) == 0 {
					return err
				}
				repoRoot, err = DiscoverRepoRoot(MustAbs(targets[0]))
				if err != nil {
					return err
				}
			}

			// No target → interactive multi-select over the repo's
			// worktrees (Tab marks, Enter deletes the marked set).
			if len(targets) == 0 {
				picked, perr := pickWorktrees(ctx, repoRoot, "select worktrees to remove")
				if perr != nil {
					return perr
				}
				if len(picked) == 0 {
					return nil
				}
				targets = picked
				// The picker self-erases on exit, so echo what it
				// concluded while it's still on the record — dim, on
				// stderr, ahead of any prompt or teardown output.
				names := make([]string, len(targets))
				for i, t := range targets {
					names[i] = filepath.Base(MustAbs(t))
				}
				ui.Hint("selected %d worktree(s): %s", len(targets), strings.Join(names, ", "))
			}

			// Batch confirmation: with several targets, hoist the
			// dirty/unpushed probes out of the per-target loop so N
			// risky picks cost ONE question instead of N sequential
			// ones (#77). --yes skips everything, as before.
			batchConfirmed := false
			if len(targets) > 1 && !c.Bool("yes") {
				confirmed, aborted, err := confirmBatchDelete(ctx, repoRoot, targets)
				if err != nil {
					return err
				}
				if aborted {
					return nil
				}
				batchConfirmed = confirmed
			}

			// Each target is torn down independently: one failure (or
			// one declined confirmation) must not strand the rest.
			var errs []error
			for _, target := range targets {
				if derr := deleteWorktreeTarget(ctx, c, repoRoot, target, batchConfirmed); derr != nil {
					errs = append(errs, derr)
				}
			}
			return errors.Join(errs...)
		},
	}
}

// confirmBatchDelete hoists the dirty/unpushed probes for a
// multi-target delete into one danger-styled confirmation. Every
// target with unregenerable state (uncommitted changes, unpushed
// commits) gets a line naming the worktree, its branch, and its
// reasons; a single ConfirmYes then covers the whole batch. Returns
// (confirmed, aborted): confirmed skips the per-target prompts, and
// aborted means the user declined — everything stays intact.
// All-clean batches return (false, false): nothing risky, so the
// delete proceeds without ceremony, exactly like the single-target
// path.
func unregenerableState(ctx context.Context, wtPath string) ([]string, error) {
	dirty, err := gitenv.HasWorkingTreeChanges(ctx, wtPath)
	if err != nil {
		return nil, fmt.Errorf("check %s for uncommitted changes before deleting it: %w", wtPath, err)
	}
	unpushed, err := gitenv.HasUnpushedCommits(ctx, wtPath)
	if err != nil {
		return nil, fmt.Errorf("check %s for unpushed commits before deleting it: %w", wtPath, err)
	}
	var reasons []string
	if dirty {
		reasons = append(reasons, "uncommitted changes")
	}
	if unpushed {
		reasons = append(reasons, "unpushed commits")
	}
	return reasons, nil
}

func confirmBatchDelete(ctx context.Context, repoRoot string, targets []string) (confirmed, aborted bool, err error) {
	type finding struct {
		path, branch string
		reasons      []string
	}
	var findings []finding
	for _, target := range targets {
		wtPath := MustAbs(target)
		p, ok, lookupErr := wt.LookupWorktree(ctx, repoRoot, target, cliSink{})
		if lookupErr != nil {
			return false, false, lookupErr
		}
		if ok {
			wtPath = p
		}
		reasons, err := unregenerableState(ctx, wtPath)
		if err != nil {
			return false, false, err
		}
		if len(reasons) == 0 {
			continue
		}
		branch, _ := gitcmd.String(ctx, wtPath, "rev-parse", "--abbrev-ref", "HEAD")
		findings = append(findings, finding{path: wtPath, branch: branch, reasons: reasons})
	}
	if len(findings) == 0 {
		return false, false, nil
	}
	_, _ = fmt.Fprintln(ui.Err, ui.Red(fmt.Sprintf("destroying %d worktree(s) with unregenerable state:", len(findings))))
	for _, f := range findings {
		label := f.path
		if f.branch != "" {
			label = fmt.Sprintf("%s (%s)", f.path, f.branch)
		}
		_, _ = fmt.Fprintf(ui.Err, "  %s — %s\n", label, strings.Join(f.reasons, ", "))
	}
	if !ui.ConfirmYes(fmt.Sprintf("destroy these %d worktrees?", len(findings))) {
		PrintInfo("aborted: all %d worktrees left intact", len(targets))
		return false, true, nil
	}
	return true, false, nil
}

// deleteWorktreeTarget tears down one worktree named by path, branch or
// slug: the cwd refusal, the dirty/unpushed confirmation, then the
// delete itself. preConfirmed suppresses the per-target confirmation
// (the multi-target batch prompt already covered it).
func deleteWorktreeTarget(ctx context.Context, c *cli.Command, repoRoot, target string, preConfirmed bool) error {
	// Confirmation lives in the CLI — wt.Delete itself is
	// non-interactive. Resolve the wtPath up front so the
	// prompt can name the target precisely; this duplicates
	// wt.Delete's lookup but the second pass inside the
	// orchestrator is cheap (sqlite query).
	wtPath := MustAbs(target)
	p, ok, err := wt.LookupWorktree(ctx, repoRoot, target, cliSink{})
	if err != nil {
		return err
	}
	if ok {
		wtPath = p
	}

	// --dry-run (#60): resolve the target, show the git state + the
	// per-engine drop plan, touch nothing. Main-worktree and cwd
	// refusals still fire — they're guards, not work.
	if c.Bool("dry-run") {
		return previewDelete(ctx, c, repoRoot, target, wtPath)
	}

	// Refuse to tear down the worktree the shell is standing in —
	// the daemon would rm -rf it under the user's feet and leave
	// the shell in a deleted directory. `worktree back --remove`
	// is the leave-and-drop flow. The main repo root is exempt
	// here: wt.Delete has its own dedicated main-worktree
	// refusal and that error message must win.
	if cwd, cwdErr := os.Getwd(); cwdErr == nil && !strings.EqualFold(filepath.Clean(wtPath), filepath.Clean(repoRoot)) {
		if top, topErr := gitWorktreeRoot(cwd); topErr == nil && top == wtPath {
			return errors.New(
				"refusing to delete the worktree you're in — use `cd \"$(treeman worktree back --remove)\"`")
		}
	}

	// Prompt policy (zsh gwtd parity): a clean, fully-pushed
	// worktree deletes without ceremony — its state is
	// regenerable. Only uncommitted changes / unpushed commits
	// warrant a stop, and then as a default-yes warning naming
	// the reason.
	if !c.Bool("yes") && !preConfirmed {
		reasons, err := unregenerableState(ctx, wtPath)
		if err != nil {
			return err
		}
		if len(reasons) > 0 {
			q := fmt.Sprintf("Worktree %s has %s — destroy?", wtPath, strings.Join(reasons, ", "))
			if !ui.ConfirmYes(q) {
				PrintInfo("aborted: worktree %s left intact", wtPath)
				return nil
			}
		}
	}

	// Name the resolved target before anything destructive happens —
	// a clean worktree fires no confirmation, so without this line a
	// wrong-slug delete is only discoverable afterwards via wt list.
	if branch, berr := gitcmd.String(ctx, wtPath, "rev-parse", "--abbrev-ref", "HEAD"); berr == nil && branch != "" {
		PrintInfo("deleting %s (branch %s) — DBs + git worktree will be dropped; follow: treeman worktree logs %s --follow",
			wtPath, branch, filepath.Base(wtPath))
	} else {
		PrintInfo("deleting %s — DBs + git worktree will be dropped; follow: treeman worktree logs %s --follow",
			wtPath, filepath.Base(wtPath))
	}

	// --foreground: anchor a teardown poll BEFORE dispatch (the row may
	// be marked deleted mid-teardown), then block on worktree:delete:end.
	if c.Bool("foreground") {
		return deleteForeground(ctx, c, repoRoot, target, wtPath)
	}

	_, err = wt.Delete(ctx, wt.DeleteRequest{
		RepoRoot: repoRoot,
		Target:   target,
		Force:    c.Bool("force"),
		Env:      CaptureInheritedEnv(),
		Detached: c.Bool("detached"),
	}, cliSink{})
	return err
}

// deleteForeground implements `wt delete --foreground` (#50): dispatch
// the teardown, then block until the daemon emits worktree:delete:end
// for the row (or the in-process teardown already finished before the
// poll started — the pre-dispatch anchor sees the terminal event
// either way).
func deleteForeground(ctx context.Context, c *cli.Command, repoRoot, target, wtPath string) error {
	st, closer, err := openLogStore(ctx)
	if err != nil {
		return err
	}
	defer closer()
	repoID, err := resolveShowRepoID(ctx, st, c.String("repo"))
	if err != nil {
		return err
	}
	row, err := worktreeRowByPath(ctx, st, repoID, wtPath)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", wtPath, err)
	}
	anchor := time.Now().UnixMilli()

	res, err := wt.Delete(ctx, wt.DeleteRequest{
		RepoRoot: repoRoot,
		Target:   target,
		Force:    c.Bool("force"),
		Env:      CaptureInheritedEnv(),
		Detached: c.Bool("detached"),
	}, cliSink{})
	if err != nil {
		return err
	}
	if res.Status == wt.DeleteInline {
		// The in-process teardown already completed synchronously.
		ui.Success("torn down: %s", wtPath)
		return nil
	}
	return pollTeardown(ctx, st, row, anchor, time.Now().Add(10*time.Minute))
}

// worktreeRowByPath resolves the live registry row for an exact path —
// the delete foreground-wait's anchor (name-based lookups don't match
// full paths).
func worktreeRowByPath(ctx context.Context, st *store.Store, repoID int64, wtPath string) (worktreeRow, error) {
	var row worktreeRow
	err := st.DB.QueryRowContext(ctx, `
		SELECT w.id, w.slug, COALESCE(w.branch,''), w.path
		FROM worktrees w
		WHERE w.repo_id = ? AND w.path = ? COLLATE NOCASE AND w.deleted_at IS NULL`,
		repoID, wtPath).Scan(&row.ID, &row.Slug, &row.Branch, &row.Path)
	if err != nil {
		return row, fmt.Errorf("no live worktree row matches %q (try `treeman worktree list`)", wtPath)
	}
	return row, nil
}

// pollTeardown blocks until the daemon emits worktree:delete:end or
// worktree:delete:error for the row anchored at `anchor` — the delete
// twin of pollFinalize. A spinner with elapsed/last-progress renders on
// a TTY; piped stderr stays silent.
func pollTeardown(ctx context.Context, st *store.Store, row worktreeRow, anchor int64, deadline time.Time) error {
	started := time.Now()
	spinner := ui.IsStderrTTY()
	frame := 0
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if spinner {
				ui.EraseLine()
			}
			return ctx.Err()
		case <-tick.C:
		}
		rows, err := st.QueryEvents(ctx, store.EventFilter{
			WorktreeID:  row.ID,
			EventTypes:  []string{store.EvtWorktreeDeleteEnd, store.EvtWorktreeDeleteError},
			SinceMs:     anchor,
			OldestFirst: true,
			Limit:       50,
		})
		if err != nil {
			if spinner {
				ui.EraseLine()
			}
			return err
		}
		for _, e := range rows {
			if spinner {
				ui.EraseLine()
			}
			switch e.EventType {
			case store.EvtWorktreeDeleteEnd:
				ui.Success("torn down: %s", row.Path)
				return nil
			case store.EvtWorktreeDeleteError:
				if e.Level == store.LevelError {
					return fmt.Errorf("teardown failed: %s", e.Message)
				}
			}
		}
		if time.Now().After(deadline) {
			if spinner {
				ui.EraseLine()
			}
			return fmt.Errorf("timed out waiting for teardown of %s", row.Path)
		}
		if spinner {
			line := fmt.Sprintf("%s waiting for teardown on %s — elapsed %s",
				ui.Cyan(ui.SpinnerFrames[frame%len(ui.SpinnerFrames)]),
				row.Slug, time.Since(started).Round(time.Second))
			ui.EraseLine()
			_, _ = fmt.Fprint(ui.Err, ui.Dim(ui.Truncate(line, ui.TermWidth())))
			frame++
		}
	}
}

// previewDelete implements `wt delete --dry-run` (#60): the resolved
// target + git state + the per-engine drop plan PreviewTeardown
// renders, with NO engine connections. Refusals that would stop the
// real delete (main worktree, cwd) still error — a preview that lies
// about being deletable is worse than none.
func previewDelete(ctx context.Context, c *cli.Command, repoRoot, _, wtPath string) error {
	if strings.EqualFold(filepath.Clean(wtPath), filepath.Clean(repoRoot)) {
		return fmt.Errorf("refusing to delete %q: it is the repo's main worktree (primary checkout), not a linked worktree", wtPath)
	}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		if top, topErr := gitWorktreeRoot(cwd); topErr == nil && top == wtPath {
			return errors.New("refusing to delete the worktree you're in — use `cd \"$(treeman worktree back --remove)\"`")
		}
	}
	branch := gitenv.DetectBranch(ctx, wtPath)
	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wtPath)
	if err != nil {
		return err
	}
	sl, err := registrySlug(ctx, repoRoot, wtPath)
	if err != nil {
		return err
	}
	dbs, err := prepare.PreviewTeardown(&cfg, repoRoot, wtPath, sl, branch)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(ui.Out).Encode(map[string]any{
			"dry_run": true, "wt_path": wtPath, "branch": branch, "databases": dbs,
		})
	}
	PrintInfo("dry run for %s (branch %s) — nothing was removed:", wtPath, branch)
	printPlanEntries(dbs)
	return nil
}

// printPlanEntries renders PreviewTeardown output as one line per
// database: engine, kind, then the namespace list (clones inlined).
func printPlanEntries(dbs []prepare.PlanEntry) {
	for _, d := range dbs {
		PrintInfo("  %s %s: %s", ui.Bold(d.Engine), ui.Dim(d.Kind), strings.Join(d.Names, ", "))
	}
}

func registrySlug(ctx context.Context, repoRoot, wtPath string) (string, error) {
	st, err := openSharedStore(ctx)
	if err != nil {
		return "", fmt.Errorf("open the treeman store to read the slug of %s: %w", wtPath, err)
	}
	var sl string
	err = st.DB.QueryRowContext(ctx, `
		SELECT COALESCE(w.slug,'') FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? COLLATE NOCASE AND w.path = ? COLLATE NOCASE AND w.deleted_at IS NULL`,
		repoRoot, wtPath).Scan(&sl)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read the registered slug of %s: %w", wtPath, err)
	}
	return sl, nil
}

// previewBranchReset implements `db reset --dry-run` (#60): the
// branch_scoped namespaces that would be dropped + re-seeded, pure
// name math — no daemon dispatch, no engine connections.
func previewBranchReset(ctx context.Context, c *cli.Command, repoRoot, wtPath string) error {
	branch := gitenv.DetectBranch(ctx, wtPath)
	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wtPath)
	if err != nil {
		return err
	}
	sl, err := registrySlug(ctx, repoRoot, wtPath)
	if err != nil {
		return err
	}
	dbs, err := prepare.PreviewBranchReset(&cfg, repoRoot, wtPath, sl, branch)
	if err != nil {
		return err
	}
	if c.String("engine") != "" {
		filtered := dbs[:0]
		for _, d := range dbs {
			if strings.EqualFold(d.Engine, c.String("engine")) {
				filtered = append(filtered, d)
			}
		}
		dbs = filtered
	}
	if c.Bool("json") {
		return json.NewEncoder(ui.Out).Encode(map[string]any{
			"dry_run": true, "wt_path": wtPath, "branch": branch, "would_drop": dbs,
		})
	}
	PrintInfo("dry run for %s (branch %s) — nothing was reset:", wtPath, branch)
	if len(dbs) == 0 {
		ui.Hint("no branch_scoped databases configured — a reset would be a no-op")
		return nil
	}
	printPlanEntries(dbs)
	return nil
}

func wtRegister() *cli.Command {
	return &cli.Command{
		Name:      "register",
		Usage:     "register a worktree path (metadata only)",
		ArgsUsage: "[path]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "branch", Aliases: []string{"b"}},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			path := "."
			if c.NArg() >= 1 {
				path = c.Args().First()
			}
			path = MustAbs(path)
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				repoRoot, err = DiscoverRepoRoot(path)
				if err != nil {
					return err
				}
			}
			branch := c.String("branch")
			task := rpc.Task{
				Type: rpc.TaskWorktreeRegister, RepoPath: repoRoot, WorktreePath: path,
				Params: map[string]string{rpc.ParamBranch: branch},
			}
			if c.Bool("json") {
				return runResultJSON(ctx, task)
			}
			payload, err := resultPayload(ctx, task)
			if err != nil {
				return err
			}
			var r struct {
				WorktreeID int64  `json:"worktree_id"`
				RepoID     int64  `json:"repo_id"`
				Slug       string `json:"slug"`
			}
			_ = json.Unmarshal(payload, &r)
			PrintInfo("worktree #%d slug=%s repo=#%d (%s)", r.WorktreeID, r.Slug, r.RepoID, repoRoot)
			return nil
		},
	}
}

func wtUnregister() *cli.Command {
	return &cli.Command{
		Name:      "unregister",
		Usage:     "mark a worktree deleted in SQLite without touching git",
		ArgsUsage: "[path]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			path := "."
			if c.NArg() >= 1 {
				path = c.Args().First()
			}
			path = MustAbs(path)
			var repoRoot string
			if r := c.String("repo"); r != "" {
				var err error
				if repoRoot, err = resolveRepo(r); err != nil {
					return err
				}
			}
			task := rpc.Task{Type: rpc.TaskWorktreeUnregister, RepoPath: repoRoot, WorktreePath: path}
			if c.Bool("json") {
				return runResultJSON(ctx, task)
			}
			if _, err := resultPayload(ctx, task); err != nil {
				return err
			}
			PrintOK("unregistered %s (git worktree untouched)", path)
			return nil
		},
	}
}

func wtList() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list active worktrees",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json"},
			&cli.BoolFlag{Name: "tsv", Usage: "machine output: one <path>\\t<branch> line per active worktree (for shell consumption)"},
			&cli.BoolFlag{Name: "with-state", Usage: "include a STATE column derived from the most recent finalize event"},
			&cli.BoolFlag{
				Name:  "with-status",
				Usage: "include a STATUS column (clean/dirty/unpushed; forks git status + rev-list per row)",
			},
			&cli.BoolFlag{Name: "with-size", Usage: "include a SIZE column (on-disk bytes; runs du per row concurrently)"},
			&cli.StringFlag{Name: "sort", Value: "id", Usage: "id | mtime (HEAD commit ts) | visited (last_visited_at)"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "scope to one repo (path)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			st, err := openDefaultStore(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			all, err := loadWtRows(ctx, st, c.String("repo"), c.String("sort"))
			if err != nil {
				return err
			}

			withStatus := c.Bool("with-status")
			withState := c.Bool("with-state")
			withSize := c.Bool("with-size")
			if err := enrichAndSortWtRows(ctx, c, st, all, withStatus, withState, withSize); err != nil {
				return err
			}

			if c.Bool("tsv") {
				for _, r := range all {
					_, _ = fmt.Fprintf(ui.Out, "%s\t%s\n", r.Path, r.Branch)
				}
				return nil
			}
			if c.Bool("json") {
				return jsonStream(all)
			}
			if len(all) == 0 {
				ui.Info("no active worktrees")
				ui.Hint("%s", "create one with: treeman worktree create <branch>")
				return nil
			}

			anyMain := false
			for _, r := range all {
				if r.IsMain {
					anyMain = true
				}
			}
			renderWtTable(all, anyMain, withStatus, withState, withSize)
			return nil
		},
	}
}

// loadWtRows queries the active-worktree rows the list table (and the
// `tui` dashboard) render. Tearing-down worktrees are excluded in SQL:
// the daemon writes delete:start the instant teardown begins but only
// flips deleted_at when the final git-remove lands, so without the
// predicate a tearing-down worktree lingers in the list for the whole
// DB-drop + hooks window. Self-healing: a failed teardown lands
// delete:error and the worktree reappears.
func loadWtRows(ctx context.Context, st *store.Store, repoFilter, sortMode string) ([]wtRow, error) {
	var (
		args    []any
		where   = "w.deleted_at IS NULL AND " + store.WorktreeNotTearingDown
		orderBy string
	)
	if repoFilter != "" {
		repoID, err := st.LookupRepoID(ctx, MustAbs(repoFilter))
		if err != nil {
			return nil, fmt.Errorf("look up repo %s: %w", MustAbs(repoFilter), err)
		}
		if repoID == 0 {
			return nil, fmt.Errorf("no repo registered at %s", repoFilter)
		}
		where += " AND w.repo_id = ?"
		args = append(args, repoID)
	}
	switch sortMode {
	case "visited":
		orderBy = "ORDER BY IFNULL(w.last_visited_at, 0) DESC, w.id"
	case "mtime", "id", "":
		orderBy = "ORDER BY w.id"
	default:
		return nil, fmt.Errorf("unknown --sort %q (id|mtime|visited)", sortMode)
	}
	//nolint:gosec // where/orderBy are fixed fragments; values are parameterized via args
	q := `SELECT w.id, w.slug, COALESCE(w.branch,'-'), w.path, COALESCE(w.last_visited_at, 0), w.is_main, r.path
		FROM worktrees w JOIN repos r ON r.id = w.repo_id WHERE ` + where + ` ` + orderBy
	rows, err := st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var all []wtRow
	for rows.Next() {
		var r wtRow
		if err := rows.Scan(&r.ID, &r.Slug, &r.Branch, &r.Path, &r.VisitedTs, &r.IsMain, &r.RepoPath); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	return all, rows.Err()
}

// enrichAndSortWtRows fills the optional columns requested by the
// list flags (state, status, HEAD ts, sizes) and applies the --sort
// mode. Extracted from wtList's action to keep it under the
// complexity gate.
func enrichAndSortWtRows(ctx context.Context, c *cli.Command, st *store.Store, all []wtRow, withStatus, withState, withSize bool) error {
	sortMode := c.String("sort")
	needHeadTs := sortMode == "mtime" || c.Bool("json") || withSize
	if err := enrichWtRows(ctx, st, all, withStatus, withState, needHeadTs, c.Bool("json") || withSize); err != nil {
		return err
	}
	if withSize {
		paths := make([]string, len(all))
		for i := range all {
			paths[i] = all[i].Path
		}
		sizes := worktreeSizes(ctx, paths)
		for i := range all {
			all[i].SizeBytes = sizes[i]
		}
	}
	if sortMode == "mtime" {
		sort.SliceStable(all, func(i, j int) bool { return all[i].HeadTs > all[j].HeadTs })
	}
	return nil
}

// wtRow is one row of `treeman worktree list` output.
type wtRow struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	RepoPath    string `json:"-"` // join column; drives the per-repo git fan-out below
	IsMain      bool   `json:"is_main"`
	HeadTs      int64  `json:"head_ts,omitempty"`
	VisitedTs   int64  `json:"visited_ts,omitempty"`
	Status      string `json:"status,omitempty"`
	StatusError string `json:"status_error,omitempty"`
	Dirty       bool   `json:"dirty,omitempty"`
	Unpushed    bool   `json:"unpushed,omitempty"`
	FinalState  string `json:"state,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"` // on-disk bytes (populated by --with-size)
}

// enrichWtRows fills in per-row HEAD timestamp, git status and finalize
// state, honoring which optional columns were requested (or JSON
// output, which always populates everything).
//
// HEAD timestamps come from ONE `git for-each-ref refs/heads` per repo
// (refs are shared across a repo's worktrees) instead of a git log fork
// per row; dirty/unpushed probes fan out concurrently like the switch
// menu; finalize states come from one batched events query instead of
// one query per row.
func enrichWtRows(ctx context.Context, st *store.Store, all []wtRow, withStatus, withState, needHeadTs, asJSON bool) error {
	if needHeadTs {
		fillHeadTimestamps(ctx, all)
	}
	if withState || asJSON {
		ids := make([]int64, len(all))
		for i := range all {
			ids[i] = all[i].ID
		}
		latest, err := st.LatestEventPerWorktree(ctx, ids, store.WorktreeStateEventTypes)
		if err != nil {
			return fmt.Errorf("read the setup state of %d worktrees: %w", len(ids), err)
		}
		for i := range all {
			all[i].FinalState = finalizeStateLabel(latest[all[i].ID])
		}
	}
	if withStatus || asJSON {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(8)
		for i := range all {
			g.Go(func() error {
				enrichRowStatus(gctx, &all[i])
				return nil
			})
		}
		_ = g.Wait()
	}
	return nil
}

// fillHeadTimestamps sets HeadTs on every row via one
// `for-each-ref refs/heads` per distinct repo (branch → committer
// date). Rows whose branch is absent (detached HEAD, odd refs) fall
// back to the per-row `git log -1` fork.
func fillHeadTimestamps(ctx context.Context, all []wtRow) {
	repos := map[string]map[string]int64{}
	for i := range all {
		r := &all[i]
		if r.RepoPath == "" {
			r.HeadTs = headCommitTs(r.Path)
			continue
		}
		tsByBranch, ok := repos[r.RepoPath]
		if !ok {
			tsByBranch = branchCommitTs(ctx, r.RepoPath)
			repos[r.RepoPath] = tsByBranch
		}
		if ts, ok := tsByBranch[r.Branch]; ok {
			r.HeadTs = ts
		} else {
			r.HeadTs = headCommitTs(r.Path)
		}
	}
}

// branchCommitTs maps branch name → HEAD commit unix timestamp for
// every local branch of the repo, in one fork.
func branchCommitTs(ctx context.Context, repoRoot string) map[string]int64 {
	out, err := gitcmd.Output(ctx, repoRoot,
		"for-each-ref", "--format=%(refname:short) %(committerdate:unix)", "refs/heads")
	if err != nil {
		return nil
	}
	m := map[string]int64{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		name, ts, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			continue
		}
		m[name] = n
	}
	return m
}

// enrichRowStatus fills the dirty/unpushed columns of one row.
func enrichRowStatus(ctx context.Context, r *wtRow) {
	dirty, dErr := worktreeDirty(ctx, r.Path)
	unpushed, uErr := gitenv.HasUnpushedCommits(ctx, r.Path)
	r.Dirty = dirty
	r.Unpushed = unpushed
	switch {
	case dErr != nil:
		r.Status = "?"
		r.StatusError = dErr.Error()
	case uErr != nil:
		r.Status = "?"
		r.StatusError = uErr.Error()
	default:
		r.Status = statusLabel(dirty, unpushed)
	}
}

// renderWtTable prints the human-readable `treeman worktree list` table.
func renderWtTable(all []wtRow, anyMain, withStatus, withState, withSize bool) {
	// MAIN column only shows up when at least one row is the
	// main wt — keeps the dominant linked-only output narrow.
	headers := []string{"ID"}
	if anyMain {
		headers = append(headers, "MAIN")
	}
	headers = append(headers, "SLUG", "BRANCH")
	if withStatus {
		headers = append(headers, "STATUS")
	}
	if withState {
		headers = append(headers, "STATE")
	}
	if withSize {
		headers = append(headers, "SIZE")
	}
	headers = append(headers, "LAST", "PATH")
	tbl := ui.NewTable(headers...)
	anyStatusErr := false
	for _, r := range all {
		cells := []string{ui.Dim(strconv.FormatInt(r.ID, 10))}
		if anyMain {
			if r.IsMain {
				cells = append(cells, ui.Cyan("★"))
			} else {
				cells = append(cells, "")
			}
		}
		cells = append(cells, ui.Cyan(r.Slug), r.Branch)
		if withStatus {
			if r.StatusError != "" {
				anyStatusErr = true
				cells = append(cells, ui.Yellow("?"))
			} else {
				cells = append(cells, colorStatus(r.Dirty, r.Unpushed))
			}
		}
		if withState {
			cells = append(cells, r.FinalState)
		}
		if withSize {
			cells = append(cells, ui.Dim(humanBytes(r.SizeBytes)))
		}
		cells = append(cells, ui.Dim(lastLabel(r.HeadTs, r.VisitedTs)), r.Path)
		tbl.Row(cells...)
	}
	tbl.SetWidth(ui.TermWidth()).Render(nil)
	if anyStatusErr {
		ui.Hint("%s", "STATUS '?' = git status failed; use --json for the per-row error")
	}
}

// headCommitTs returns the worktree's HEAD commit unix timestamp (0 on
// error). Forks `git -C path log -1 --format=%ct HEAD` — cheap enough
// per row for the dozen-or-so worktrees a typical repo carries.
func headCommitTs(path string) int64 {
	out, err := gitcmd.String(context.Background(), path, "log", "-1", "--format=%ct", "HEAD")
	if err != nil {
		return 0
	}
	var ts int64
	_, _ = fmt.Sscanf(out, "%d", &ts)
	return ts
}

// worktreeDirty reports whether the worktree differs from HEAD in any
// way, untracked files included (the destructive-op definition — the
// column feeds delete decisions and dirty markers). Returns false + error on
// unreadable git state (treated as not-dirty so a missing repo doesn't
// produce a misleading `*` marker).
func worktreeDirty(ctx context.Context, path string) (bool, error) {
	return gitenv.HasWorkingTreeChanges(ctx, path)
}

func statusLabel(dirty, unpushed bool) string {
	switch {
	case dirty && unpushed:
		return "dirty+unpushed"
	case dirty:
		return "dirty"
	case unpushed:
		return "unpushed"
	}
	return "clean"
}

func colorStatus(dirty, unpushed bool) string {
	switch {
	case dirty && unpushed:
		return ui.Red("dirty+unpushed")
	case dirty:
		return ui.Yellow("dirty")
	case unpushed:
		return ui.Yellow("unpushed")
	}
	return ui.Green("clean")
}

// lastLabel renders the most-recent-activity timestamp for the LAST
// column. Prefers HEAD commit ts (set when sorting by mtime or in
// JSON output); falls back to last_visited_at when only that is
// populated; "—" when neither is known.
func lastLabel(headTs, visitedTs int64) string {
	ts := headTs
	if ts == 0 {
		ts = visitedTs / 1000
	}
	if ts == 0 {
		return "—"
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return time.Unix(ts, 0).Format("2006-01-02")
	}
}

func wtFinalize() *cli.Command {
	return &cli.Command{
		Name:      "finalize",
		Usage:     "rerun setup + prepare for a worktree (default via daemon; --local runs inline)",
		ArgsUsage: "[path|slug|branch]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "local", Usage: "run setup + prepare in this process instead of dispatching to the daemon"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() > 1 {
				return fmt.Errorf("finalize takes at most one positional (path|slug|branch); got %d", c.NArg())
			}
			arg := ""
			if c.NArg() == 1 {
				arg = c.Args().First()
			}
			repoRoot, repoErr := resolveRepo(c.String("repo"))

			wtPath, repoRoot, err := resolveFinalizeTarget(ctx, arg, repoRoot, repoErr)
			if err != nil {
				return err
			}

			if c.Bool("local") {
				return runLocalFinalizeFlow(ctx, repoRoot, wtPath)
			}

			// Dispatch to the daemon when reachable, else run the tail
			// in-process — `wt finalize` works daemon-less.
			switch resp := submitPlan(ctx, finalizeRequest(repoRoot, wtPath)); resp.Kind {
			case rpc.KindPlanQueued:
				PrintOK("queued: setup + prepare detached to daemon — follow with `treeman logs tail --follow`")
			case rpc.KindError:
				return fmt.Errorf("finalize: %s", resp.Message)
			default:
				PrintOK("finalize complete")
			}
			return nil
		},
	}
}

// resolveFinalizeTarget maps the positional arg to a concrete worktree
// path, discovering the repo root when `--repo` wasn't supplied. Returns
// the resolved (wtPath, repoRoot). `repoErr` is the error from the
// initial resolveRepo attempt; a non-nil value means no repo was scoped.
func resolveFinalizeTarget(ctx context.Context, arg, repoRoot string, repoErr error) (string, string, error) {
	var wtPath string
	switch arg {
	case "", ".":
		wtPath = MustAbs(".")
		if repoErr != nil {
			var err error
			repoRoot, err = DiscoverRepoRoot(wtPath)
			if err != nil {
				return "", "", err
			}
		}
	default:
		abs := MustAbs(arg)
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			wtPath = abs
			if repoErr != nil {
				var derr error
				repoRoot, derr = DiscoverRepoRoot(wtPath)
				if derr != nil {
					return "", "", derr
				}
			}
		} else {
			// Not a directory — try resolving as a registered
			// slug/branch/basename. Without a repo to scope the
			// lookup, an unknown token would silently fabricate a
			// path under cwd; refuse instead.
			if repoErr != nil {
				return "", "", fmt.Errorf(
					"cannot resolve %q: not a directory and no repo to scope a slug lookup (cd into the repo or pass --repo)",
					arg,
				)
			}
			p, ok, err := wt.LookupWorktree(ctx, repoRoot, arg, cliSink{})
			if err != nil {
				return "", "", err
			}
			if !ok {
				return "", "", fmt.Errorf("no worktree matches %q (expected a path or registered slug/branch)", arg)
			}
			wtPath = p
		}
	}
	return wtPath, repoRoot, nil
}

// runLocalFinalizeFlow runs setup + prepare inline (used by `wt
// finalize --local`). Loads its own resolved
// config + store handle since the parent has already exited by the time
// this runs. Routes through wt.ResolveIdentity so `wt finalize . --local`
// at the repo root picks up the main-wt overlay/slug instead of producing
// a path-hash slug that would corrupt the main row.
func runLocalFinalizeFlow(ctx context.Context, repoRoot, wtPath string) error {
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return err
	}
	st, err := openDefaultStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return fmt.Errorf("register repo %s: %w", repoRoot, err)
	}
	branch := detectBranchOfWorktree(wtPath)
	id, err := wt.ResolveIdentity(ctx, st, &cfg, repoRoot, wtPath, branch, repoID)
	if err != nil {
		return err
	}
	return wt.RunLocalFinalize(
		ctx,
		&cfg,
		repoRoot,
		wtPath,
		id.Slug,
		id.IsMain,
		st,
		repoID,
		id.WtID,
		CaptureInheritedEnv(),
		false,
		cliSink{},
	)
}

// resolveRepo returns the explicit `--repo` if set, else discovers
// from cwd.
func resolveRepo(override string) (string, error) {
	if override != "" {
		return MustAbs(override), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return DiscoverRepoRoot(cwd)
}

// wtBack — `treeman worktree back`. Prints the main repo root for the
// current cwd. With `--remove`, also deletes the current linked
// worktree if it is clean (`git status --porcelain` empty) and has
// no commits ahead of upstream.
//
// Used by the zsh shim: `cd "$(treeman worktree back)"`. The remove path
// shells out to `treeman worktree delete` so the standard teardown +
// daemon RPC flow runs.
func wtBack() *cli.Command {
	return &cli.Command{
		Name:  "back",
		Usage: "print main repo path (with --remove, drop current worktree if clean)",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "remove", Usage: "delete current worktree if clean, fully pushed and not in use by another process"},
			&cli.BoolFlag{Name: "force", Usage: "with --remove: pass --force to delete"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			repoRoot, err := DiscoverRepoRoot(cwd)
			if err != nil {
				return err
			}

			if !c.Bool("remove") {
				fmt.Println(repoRoot)
				return nil
			}

			// Remove path. Determine the worktree to drop.
			if !gitenv.IsLinkedWorktree(cwd) {
				// Already in main repo — nothing to remove.
				fmt.Fprintln(os.Stderr, "not in a linked worktree; --remove ignored")
				fmt.Println(repoRoot)
				return nil
			}

			// Resolve the worktree root (cwd may be a subdirectory).
			wtRoot, err := gitWorktreeRoot(cwd)
			if err != nil {
				return err
			}

			// The dirty/unpushed gate is NOT bypassable — `--remove` must
			// never destroy work (the zsh `gwtc` contract; the claude
			// wrapper runs this automatically on exit). `--force` only
			// forwards to the git-level removal below, where it exists to
			// get past gitignored build artifacts (vendor/, node_modules/),
			// not past uncommitted changes. Explicitly nuking a dirty
			// worktree is `treeman worktree delete --force`'s job.
			// Untracked files count: removing the worktree destroys them too.
			dirty, err := gitenv.HasWorkingTreeChanges(ctx, wtRoot)
			if err != nil {
				ui.Warn("git status failed: %v; --remove aborted", err)
				fmt.Println(repoRoot)
				return nil
			}
			if dirty {
				fmt.Fprintln(os.Stderr, "keeping worktree (uncommitted changes)")
				fmt.Println(repoRoot)
				return nil
			}
			unpushed, err := gitenv.HasUnpushedCommits(ctx, wtRoot)
			if err != nil {
				ui.Warn("checking for unpushed commits failed: %v; --remove aborted", err)
				fmt.Println(repoRoot)
				return nil
			}
			if unpushed {
				fmt.Fprintln(os.Stderr, "keeping worktree (commits ahead of upstream)")
				fmt.Println(repoRoot)
				return nil
			}

			// Print main repo path FIRST so the caller can `cd "$(…)"`;
			// stdout must carry only the path. The delete below routes
			// its status through ui.Out, so redirect ui.Out to stderr
			// for that call to keep stdout path-only.
			fmt.Println(repoRoot)

			// Leave the worktree BEFORE delegating: wt delete refuses to
			// tear down the worktree the process is standing in (its
			// cwd-guard), and here removing the current worktree is the
			// whole point — the shell cd's to the path we just printed.
			// Mirrors the old zsh gwtd, which cd'd to the root first.
			if chdirErr := os.Chdir(repoRoot); chdirErr != nil {
				ui.Warn("chdir %s failed: %v; skipping --remove", repoRoot, chdirErr)
				return nil
			}

			// Delegate to wt delete to keep teardown logic in one
			// place. Errors are surfaced on stderr; we don't exit
			// non-zero because the caller already changed directory.
			// --yes is passed: wt back has already verified the
			// worktree is clean / not ahead of upstream above, and
			// the caller has already cd'd to repoRoot so an
			// interactive prompt would arrive after the cd and
			// confuse the user.
			argv := []string{"delete", "--yes", wtRoot, "--repo", repoRoot}
			if c.Bool("force") {
				argv = append(argv, "--force")
			}
			prevOut := ui.Out
			ui.Out = os.Stderr
			err = wtDelete().Run(ctx, argv)
			ui.Out = prevOut
			if inUse := (*wt.InUseError)(nil); errors.As(err, &inUse) {
				fmt.Fprintf(os.Stderr, "keeping worktree (%v)\n", inUse)
				return nil
			}
			if err != nil {
				// wt delete already rendered its own error lines; this is
				// the back-level echo that the remove step failed.
				ui.Error("wt delete failed: %v", err)
			}
			return nil
		},
	}
}

// gitWorktreeRoot returns the top-level directory of the worktree
// containing `start`. Wraps `git -C start rev-parse --show-toplevel`.
func gitWorktreeRoot(start string) (string, error) {
	out, err := gitcmd.String(context.Background(), start, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return out, nil
}

// wtPrev — `treeman worktree prev`. Prints the path of the previously-
// visited worktree for the current repo (most recent
// last_visited_at, excluding cwd). The timestamp lives in SQLite so
// toggling works across shells.
func wtPrev() *cli.Command {
	return &cli.Command{
		Name:  "prev",
		Usage: "print previously-visited worktree (registry-tracked; cross-shell)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			st, err := openDefaultStore(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			repoID, err := st.LookupRepoID(ctx, repoRoot)
			if err != nil {
				return fmt.Errorf("look up repo %s: %w", repoRoot, err)
			}
			if repoID == 0 {
				return fmt.Errorf("repo %s not registered yet", repoRoot)
			}
			cwd, _ := os.Getwd()
			cwdTop, _ := gitWorktreeRoot(cwd)
			path, ok, err := st.PrevVisitedWorktree(ctx, repoID, cwdTop)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no previously-visited worktree on record for %s", repoRoot)
			}
			touchVisitedByPath(ctx, path)
			fmt.Println(path)
			return nil
		},
	}
}

// wtGo — `treeman worktree go <name-or-branch>`. The navigation verb
// (absorbs the former `wt resolve`); `worktree switch` is the separate
// always-a-worktree landing policy.
//
// Default (pure resolve, no git side effects): fuzzy-match an existing
// worktree (slug/branch/basename), then exact-branch registry lookup,
// then a filesystem fallback. Prints the resolved path; exits nonzero
// on miss. With `--create`, spawns the worktree when nothing matches.
//
// With `--checkout`: branch checkout auto-routing (exposes the policy
// zsh's `_checkout_branch_or_worktree` implements with N git forks):
//  1. Branch already live in another worktree → print that path.
//  2. cwd is in a linked worktree AND main repo is clean → run
//     git checkout (or -b) in main, print main.
//  3. cwd is in a linked worktree AND main is dirty → delegate to
//     `wt create` so the new branch lands in a fresh worktree.
//  4. Else (main or standalone clone) → git checkout in cwd's repo
//     root, print that path.
//
// Always prints exactly one line on stdout (the destination
// directory). Status / warnings go to stderr. Touches
// last_visited_at on every successful resolution so `wt prev` works.
func wtGo() *cli.Command {
	return &cli.Command{
		Name:      "go",
		Usage:     "resolve/create/checkout a worktree by name or branch (use as cd \"$(treeman worktree go …)\")",
		ArgsUsage: "<name-or-branch>",
		Description: `The navigation primitive: pure path resolution by default, --create to
spawn the worktree, --checkout for full branch routing (the ` + "`git switch`" + ` policy,
which may check out in the main repo). To always land in a worktree, use
` + "`worktree switch`" + `.`,
		ShellComplete: worktreeArgComplete,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "create", Usage: "create the worktree if nothing matches"},
			&cli.BoolFlag{
				Name:  "checkout",
				Usage: "git checkout the branch (auto-routes main vs new worktree) instead of pure path resolution",
			},
			&cli.StringFlag{Name: "from", Usage: "base branch (with --create/--checkout)"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "no-fetch", Usage: "skip the pre-checkout `git fetch origin <base>`"},
			foregroundFlag(),
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman worktree go <name-or-branch>")
			}
			target := c.Args().First()
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			opts := spawnOpts{from: c.String("from"), noFetch: c.Bool("no-fetch"), foreground: c.Bool("foreground")}

			if c.Bool("checkout") {
				return goCheckout(ctx, repoRoot, target, c.Bool("create"), opts)
			}

			// Pure resolve: fuzzy worktree lookup, then exact branch.
			path, ok, err := wt.LookupWorktree(ctx, repoRoot, target, cliSink{})
			if err != nil {
				return err
			}
			if ok {
				landExisting(ctx, path)
				return nil
			}
			path, ok, err = registryWorktreeForBranch(ctx, repoRoot, target)
			if err != nil {
				return err
			}
			if ok {
				landExisting(ctx, path)
				return nil
			}

			cfg, err := resolve.LoadResolved(repoRoot)
			if err != nil {
				return err
			}
			// Filesystem fallback — but only for directories git itself
			// knows as linked worktrees. A leftover non-git dir under
			// .worktrees/ (e.g. node_modules/.cache from a torn-down
			// worktree) must NOT resolve: git commands there would run
			// against the main checkout. Registered worktrees already
			// matched above; teardown-guarding is likewise covered there
			// (only registered rows tear down).
			fsCandidate := filepath.Join(wt.WorktreesRoot(cfg, repoRoot), target)
			if isGitWorktreeDir(ctx, repoRoot, fsCandidate) {
				fmt.Println(fsCandidate)
				return nil
			}

			if !c.Bool("create") {
				return fmt.Errorf(
					"no worktree matches %q (try `treeman worktree go %s --create`, or --checkout to switch branches)",
					target,
					target,
				)
			}

			// --create → spawn the worktree, then print its path.
			return goSpawnWorktree(ctx, repoRoot, target, opts)
		},
	}
}

// goCheckout runs the branch-checkout auto-routing for `wt go --checkout`.
// Prints exactly one line (the destination directory) on stdout; status
// goes to stderr.
func goCheckout(ctx context.Context, repoRoot, branch string, create bool, opts spawnOpts) error {
	// (1) Already live in some worktree (other than cwd)? Registry
	// first, then git's own view (worktrees added outside treeman).
	path, ok, err := liveWorktreeForBranch(ctx, repoRoot, branch)
	if err != nil {
		return err
	}
	if ok {
		cwd, _ := os.Getwd()
		cwdTop, _ := gitWorktreeRoot(cwd)
		if path != cwdTop {
			landExisting(ctx, path)
			return nil
		}
	}

	mode, base := resolveGoMode(ctx, repoRoot, branch, opts.from, create, opts.noFetch)

	cwd, _ := os.Getwd()
	cwdTop, _ := gitWorktreeRoot(cwd)
	inLinked := cwdTop != "" && gitenv.IsLinkedWorktree(cwdTop)

	runCheckoutIn := func(dir string) error {
		args := []string{"checkout"}
		// Same tracking rule as worktree create: a branch forked off
		// another base records it in git config instead of adopting
		// `origin/<base>` as its upstream (see gitcmd.RecordBranchBase).
		forked := mode == "create" && base != "" && strings.TrimPrefix(base, "origin/") != branch
		if mode == "create" {
			if forked {
				args = append(args, "--no-track")
			}
			args = append(args, "-b", branch)
			if base != "" {
				args = append(args, base)
			}
		} else {
			args = append(args, branch)
		}
		// stdout goes to stderr — `wt go` reserves stdout for
		// the worktree path (consumed by `cd $(treeman worktree go …)`).
		if err := gitcmd.RunPiped(ctx, dir, os.Stderr, os.Stderr, args...); err != nil {
			return err
		}
		if forked {
			gitcmd.RecordBranchBase(ctx, dir, branch, base)
		}
		return nil
	}

	// (4) Not in a linked worktree → checkout in cwd's repo root.
	if !inLinked {
		if err := runCheckoutIn(repoRoot); err != nil {
			return err
		}
		touchVisitedByPath(ctx, repoRoot)
		fmt.Println(repoRoot)
		return nil
	}

	// (2) main clean → checkout there.
	mainClean, _ := gitenv.IsWorktreeClean(ctx, repoRoot)
	if mainClean {
		if err := runCheckoutIn(repoRoot); err != nil {
			return err
		}
		touchVisitedByPath(ctx, repoRoot)
		fmt.Println(repoRoot)
		return nil
	}

	// (3) main dirty → spawn a fresh worktree.
	return goSpawnWorktree(ctx, repoRoot, branch, opts)
}

// resolveGoMode decides whether `wt go` checks out an existing branch or
// creates a new one, and resolves the base ref. A create that already
// exists locally degrades to checkout. When creating off a base, an
// optional pre-fetch keeps the base from seeding off a stale local ref.
func resolveGoMode(ctx context.Context, repoRoot, branch, from string, create, noFetch bool) (mode, base string) {
	mode = "checkout"
	base = from
	if create {
		mode = "create"
		if wt.RefExistsLocal(ctx, repoRoot, branch) {
			mode = "checkout"
			base = ""
		}
	}

	// Optional pre-fetch so a stale local base doesn't seed the new
	// branch off yesterday's commit. Same heuristic as zsh's
	// _resolve_base.
	if mode == "create" && base != "" && !noFetch {
		_ = gitcmd.RunPiped(ctx, repoRoot, nil, nil, "fetch", "origin", base, "--quiet")
		if wt.RefExistsRemote(ctx, repoRoot, base) {
			base = "origin/" + base
		}
	}
	return mode, base
}

// goSpawnWorktree creates a fresh worktree for <branch> and prints its
// resolved path on stdout (create status/ports stream to stderr). A
// foreground create whose setup fails prints no path and fails, so a
// cd shim never lands in a half-provisioned worktree.
func goSpawnWorktree(ctx context.Context, repoRoot, branch string, opts spawnOpts) error {
	// Create's status lines go to stderr — stdout is reserved for the
	// destination path (cd "$(treeman worktree go …)").
	prevOut := ui.Out
	ui.Out = os.Stderr
	path, err := spawnWorktree(ctx, repoRoot,
		rpc.WorktreeCreateArgs{Branch: branch, From: opts.from, NoFetch: opts.noFetch}, opts.foreground)
	ui.Out = prevOut
	if err != nil {
		return err
	}
	touchVisitedByPath(ctx, path)
	fmt.Println(path)
	return nil
}

// landExisting prints an existing worktree's path for the cd shim and
// warns on stderr when that worktree's last setup failed, so switching
// into it doesn't hide the failure (#129).
func landExisting(ctx context.Context, path string) {
	touchVisitedByPath(ctx, path)
	msg, failed, err := failedSetup(ctx, path)
	if err != nil {
		PrintWarn("could not check the last setup of %s: %v", path, err)
	}
	if failed {
		PrintWarn("last setup of %s failed: %s", path, msg)
		PrintHint("fix the cause, then run `treeman worktree finalize` in it")
	}
	fmt.Println(path)
}

// failedSetup reports whether the latest finalize of the worktree at
// path failed, with its error message.
func failedSetup(ctx context.Context, path string) (msg string, failed bool, err error) {
	st, err := openSharedStore(ctx)
	if err != nil {
		return "", false, fmt.Errorf("open the treeman store: %w", err)
	}
	row, err := st.LookupActiveWorktreeByPath(ctx, path)
	if err != nil {
		return "", false, fmt.Errorf("look up the worktree at %s: %w", path, err)
	}
	if row.ID == 0 {
		return "", false, nil
	}
	last, err := st.LatestWorktreeStateEvent(ctx, row.ID)
	if err != nil {
		return "", false, fmt.Errorf("read the setup state of %s: %w", path, err)
	}
	state, _ := store.DeriveWorktreeState(last)
	return last.Message, state == store.StateError, nil
}

// registryWorktreeForBranch finds the live worktree on <branch> for
// <repoRoot>. Returns ("", false) when no row matches. Branch is an
// exact match on `worktrees.branch` — the column is populated by
// `wt create` / `wt register --branch`.
func registryWorktreeForBranch(ctx context.Context, repoRoot, branch string) (string, bool, error) {
	st, err := openSharedStore(ctx)
	if err != nil {
		return "", false, fmt.Errorf("open the treeman store to find the worktree of %s on %s: %w", repoRoot, branch, err)
	}
	//nolint:gosec // WorktreeNotTearingDown is a compile-time constant fragment
	row := st.DB.QueryRowContext(ctx, `
		SELECT w.path FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? COLLATE NOCASE AND w.deleted_at IS NULL AND w.branch = ?
		AND `+store.WorktreeNotTearingDown+`
		ORDER BY w.id DESC LIMIT 1`, repoRoot, branch)
	var p string
	switch err := row.Scan(&p); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("find the worktree of %s on %s: %w", repoRoot, branch, err)
	}
	return p, true, nil
}

func touchVisitedByPath(ctx context.Context, path string) {
	st, err := openSharedStore(ctx)
	if err != nil {
		PrintWarn("could not record the visit to %s: open the treeman store: %v", path, err)
		return
	}
	if err := st.TouchWorktreeVisitedByPath(ctx, path); err != nil {
		PrintWarn("could not record the visit to %s: %v", path, err)
	}
}

// openSharedStore is the CLI-side handle: one shared store per
// process (see store.OpenShared) instead of every helper opening —
// and migration-probing — its own.
func openSharedStore(ctx context.Context) (*store.Store, error) {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return nil, err
	}
	return store.OpenShared(ctx, dbPath)
}
