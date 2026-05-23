package cmd

import (
	"context"
	"fmt"
	"io"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/patcher"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
	"github.com/stubbedev/treeman/internal/ui"
)

// WorktreeCmd — `treeman wt {create,delete,register,unregister,list,finalize}`.
func WorktreeCmd() *cli.Command {
	return &cli.Command{
		Name:    "worktree",
		Aliases: []string{"wt"},
		Usage:   "worktree lifecycle",
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
			wtSwitch(),
			wtBack(),
			wtResolve(),
			wtPrev(),
			wtGo(),
		},
	}
}

func wtCreate() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Aliases:   []string{"new"},
		Usage:     "create a worktree end-to-end",
		ArgsUsage: "<branch>",
		Description: `Creates a linked worktree, patches the env files, registers it
in SQLite, then dispatches postcreate hooks + prepare to the daemon.

Examples:
  treeman wt create PROJ-1234
  treeman wt create feature/x --from origin/develop
  treeman wt create hotfix --foreground   # block on hooks + prepare
  cd "$(treeman wt create feat --print-path)"`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "base branch"},
			&cli.StringFlag{Name: "path", Usage: "explicit worktree path"},
			&cli.StringFlag{Name: "repo", Usage: "repo root override"},
			&cli.BoolFlag{Name: "skip-hooks"},
			&cli.BoolFlag{Name: "skip-prepare"},
			&cli.BoolFlag{Name: "foreground", Usage: "force fg execution of postcreate + prepare"},
			&cli.BoolFlag{Name: "no-fetch", Usage: "skip the pre-create `git fetch origin <base>` (defaults on so new branches pick up upstream commits)"},
			&cli.BoolFlag{Name: "print-path", Usage: "print only the new worktree path on stdout; status lines redirect to stderr (enables `cd \"$(treeman wt create …)\"`)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt create <branch>")
			}
			branch := c.Args().First()

			// When --print-path is set the shell idiom is
			// `cd "$(treeman wt create …)"`; the new path is the
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
			cfg, err := resolve.LoadResolved(repoRoot)
			if err != nil {
				return err
			}

			wtPath := c.String("path")
			if wtPath == "" {
				wtPath = filepath.Join(resolveWorktreesRoot(cfg, repoRoot), branch)
			} else if !filepath.IsAbs(wtPath) {
				wtPath = filepath.Join(repoRoot, wtPath)
			}
			if _, err := os.Stat(wtPath); err == nil {
				// Idempotent path: when the dest already exists and
				// matches what we'd create (linked worktree on the
				// requested branch, registered in SQLite), treat the
				// call as a no-op so scripts can retry safely.
				if isMatchingExistingWorktree(ctx, repoRoot, wtPath, branch) {
					PrintInfo("worktree already exists at %s on %s — no-op", wtPath, branch)
					if printPathOnly {
						fmt.Fprintln(os.Stdout, wtPath)
					}
					return nil
				}
				return fmt.Errorf("destination path already exists: %s", wtPath)
			}
			if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
				return err
			}

			// Git worktree add. Default base = origin/HEAD if -b
			// missing. Pre-fetch + prefer origin/<base> so a stale
			// local ref doesn't silently base the new branch on
			// yesterday's commit (opt-out via --no-fetch).
			base := c.String("from")
			if base == "" {
				base = detectDefaultBranch(repoRoot)
			}
			branchExists := gitcmd.Exists(ctx, repoRoot, "refs/heads/"+branch)
			if !branchExists && !c.Bool("no-fetch") {
				_ = gitcmd.RunPiped(ctx, repoRoot, nil, nil, "fetch", "origin", base, "--quiet")
				if refExistsRemote(repoRoot, base) {
					base = "origin/" + base
				}
			}
			var gitArgs []string
			if branchExists {
				gitArgs = []string{"worktree", "add", wtPath, branch}
			} else {
				gitArgs = []string{"worktree", "add", "-b", branch, wtPath, base}
			}
			if err := gitcmd.RunPiped(ctx, repoRoot, os.Stdout, os.Stderr, gitArgs...); err != nil {
				return fmt.Errorf("git worktree add: %w", err)
			}
			wtPath = MustAbs(wtPath)

			// worktrees.links — read-only symlinks from main into the
			// new worktree (vendor, node_modules, …). Glob meta-chars
			// expand against repoRoot. Idempotent: existing dst is skipped.
			if err := bringInFiles(repoRoot, wtPath, cfg.Worktrees.Links, "link"); err != nil {
				return err
			}
			// worktrees.copies — per-worktree copies so patches can
			// mutate per-branch without affecting main.
			if err := bringInFiles(repoRoot, wtPath, cfg.Worktrees.Copies, "copy"); err != nil {
				return err
			}

			sl := slug.For(wtPath, branch)
			tplCtx := template.FromSlug(sl)

			// Top-level `patches:` block — dotenv, phpunit.xml, yaml,
			// or json rewrites with per-worktree values + optional
			// git skip-worktree so the rewrite stays out of git status.
			for _, p := range cfg.Patches {
				res, err := patcher.Apply(p, wtPath, tplCtx)
				if err != nil {
					return err
				}
				if res.Outcome == patcher.Updated {
					fmt.Printf("patched %s (%s)\n", filepath.Join(wtPath, res.File), res.Driver)
				}
			}

			// Store register.
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
			if err != nil {
				return err
			}
			wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, branch)
			if err != nil {
				return err
			}
			PrintOK("created worktree #%d slug=%s path=%s", wtID, sl.Value, wtPath)

			if c.Bool("skip-hooks") {
				if printPathOnly {
					fmt.Fprintln(os.Stdout, wtPath)
				}
				return nil
			}

			env := CaptureInheritedEnv()

			// precreate is the sync phase.
			if len(cfg.Hooks.Precreate) > 0 {
				started := hooks.EmitHookStart(ctx, st, repoID, wtID, "precreate", len(cfg.Hooks.Precreate))
				out, err := hooks.RunPrecreateHooks(ctx, cfg.Hooks.Precreate, repoRoot, wtPath, sl.Value, env)
				hooks.PersistOutcome(ctx, st, repoID, wtID, "precreate", started, time.Now().UnixMilli(), out)
				if err != nil {
					return err
				}
				fmt.Printf("precreate: exit=%d\n", out.AggregateExitCode)
				if out.AggregateExitCode != 0 {
					// Surface the first failing group so the user
					// gets a log path and exit code without having
					// to grep .treeman-hooks/ by hand.
					for _, g := range out.Groups {
						if g.ExitCode != 0 {
							if g.LogPath != "" {
								return fmt.Errorf("precreate failed (exit %d) — see %s", g.ExitCode, g.LogPath)
							}
							return fmt.Errorf("precreate failed (exit %d): %s", g.ExitCode, g.Command)
						}
					}
					return fmt.Errorf("precreate failed (aggregate exit %d)", out.AggregateExitCode)
				}
			}

			// Async via daemon when async_create is true (default).
			// On daemon failure we no longer fall through to a blocking
			// foreground tail: that used to leave the interactive shell
			// hanging for minutes on composer install + dump load +
			// migrate. Instead we (a) try to bring the daemon back up,
			// (b) failing that, detach a setsid child running `treeman
			// wt finalize --local <wt>` and return immediately.
			asyncCreate := cfg.Worktrees.AsyncCreate == nil || *cfg.Worktrees.AsyncCreate
			needsWork := len(cfg.Hooks.Postcreate) > 0 || (!c.Bool("skip-prepare") && len(cfg.Databases) > 0)
			if asyncCreate && !c.Bool("foreground") && !c.Bool("skip-prepare") && needsWork {
				if queued := dispatchFinalize(ctx, repoRoot, wtPath, env); queued {
					if printPathOnly {
						fmt.Fprintln(os.Stdout, wtPath)
					}
					return nil
				}
				if logPath, err := detachLocalFinalize(wtPath, repoRoot); err == nil {
					PrintOK("queued: postcreate + prepare detached (daemon unreachable — log: %s)", logPath)
					if printPathOnly {
						fmt.Fprintln(os.Stdout, wtPath)
					}
					return nil
				} else {
					PrintWarn("detach failed (%v); falling back to foreground", err)
				}
			}

			// Foreground tail (--foreground was set, or detach failed).
			if err := runLocalFinalize(ctx, &cfg, repoRoot, wtPath, sl, st, repoID, wtID, env, c.Bool("skip-prepare")); err != nil {
				return err
			}
			if printPathOnly {
				fmt.Fprintln(os.Stdout, wtPath)
			}
			return nil
		},
	}
}

// dispatchFinalize attempts to hand postcreate + prepare to the
// daemon. Returns true when the daemon successfully queued the work.
// On the first RPC error this also tries `ensureDaemon` and retries
// once, so a daemon that's merely been signalled-but-not-yet-up gets
// a chance to come back before we resort to detaching a child.
func dispatchFinalize(ctx context.Context, repoRoot, wtPath string, env map[string]string) bool {
	req := rpc.Request{
		Method: rpc.MethodWorktreeFinalize,
		WorktreeFinalize: &rpc.WorktreeFinalizeArgs{
			RepoPath:     repoRoot,
			WorktreePath: wtPath,
			InheritedEnv: env,
		},
	}
	if resp, err := rpc.Call(ctx, req); err == nil {
		if resp.Kind == rpc.KindWorktreeFinalizeQueued {
			PrintOK("queued: postcreate + prepare detached to daemon — follow with `treeman logs tail --follow`")
			return true
		}
		if resp.Kind == rpc.KindError {
			PrintWarn("daemon: %s", resp.Message)
		}
	} else {
		PrintWarn("daemon RPC failed (%v); trying daemon restart", err)
		if startErr := ensureDaemon(ctx); startErr == nil {
			if resp, err := rpc.Call(ctx, req); err == nil && resp.Kind == rpc.KindWorktreeFinalizeQueued {
				PrintOK("queued: postcreate + prepare detached to daemon (auto-restarted)")
				return true
			}
		}
	}
	return false
}

func wtDelete() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Aliases:   []string{"rm"},
		Usage:     "delete a worktree end-to-end",
		ArgsUsage: "<path-or-branch>",
		Description: `Runs predelete hooks + DB teardown + git worktree remove, then
removes the registry row. The teardown is dispatched to the daemon
so the shell returns immediately; pass --foreground to block.

Examples:
  treeman wt delete PROJ-1234
  treeman wt delete /path/to/wt --force      # remove stale registry entry
  treeman wt delete feature/x --foreground   # block on teardown`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo"},
			&cli.BoolFlag{Name: "force", Aliases: []string{"f"}},
			&cli.BoolFlag{Name: "foreground", Usage: "force fg execution of predelete + teardown + git remove"},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt delete <path-or-branch>")
			}
			target := c.Args().First()

			// Resolve the repo root first (--repo flag > cwd walk-up >
			// fall back to target-as-path). Needed before the branch /
			// slug lookup below so the SQLite query is scoped correctly.
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				repoRoot, err = DiscoverRepoRoot(MustAbs(target))
				if err != nil {
					return err
				}
			}

			// Resolve target → worktree path. Branch / slug / basename
			// lookup wins over the literal `MustAbs(target)`
			// interpretation: `treeman wt delete feature/foo` used to
			// silently register a phantom worktree at $PWD/feature/foo
			// because MustAbs was applied unconditionally. Now we
			// consult the registry first, only falling back to
			// path-as-typed when no match is found. With --force we
			// allow the path-as-typed fallback even when the directory
			// is gone (cleaning up a stale registry entry).
			var wtPath string
			if p, ok := lookupWorktree(ctx, repoRoot, target); ok {
				wtPath = p
			} else {
				wtPath = MustAbs(target)
				if _, statErr := os.Stat(wtPath); statErr != nil && !c.Bool("force") {
					return fmt.Errorf("no worktree matches %q in %s (use --force to remove a stale registry entry)", target, repoRoot)
				}
			}

			cfg, err := resolve.LoadResolved(repoRoot)
			if err != nil {
				return err
			}
			env := CaptureInheritedEnv()

			// Destructive: predelete hooks + DROP DATABASE × N +
			// git worktree remove. Confirm with the user on a TTY;
			// scripts and `--yes` skip the prompt.
			if !c.Bool("yes") {
				q := fmt.Sprintf("delete worktree %s and drop its databases?", wtPath)
				if !ui.Confirm(q) {
					return fmt.Errorf("aborted")
				}
			}

			// On daemon failure we don't run teardown synchronously
			// any more — DROP DATABASE × N + git worktree remove on a
			// Laravel-sized vendor/ can take 10+ seconds. Try the
			// daemon, attempt auto-restart on failure, and if that
			// still misses, detach a setsid child running the same
			// `--foreground` codepath so the user's shell returns
			// immediately.
			asyncDelete := cfg.Worktrees.AsyncDelete == nil || *cfg.Worktrees.AsyncDelete
			if asyncDelete && !c.Bool("foreground") {
				if queued := dispatchTeardown(ctx, repoRoot, wtPath, c.Bool("force"), env); queued {
					return nil
				}
				if logPath, err := detachLocalDelete(wtPath, repoRoot, c.Bool("force")); err == nil {
					PrintOK("queued: predelete + DB teardown + git remove detached (daemon unreachable — log: %s)", logPath)
					return nil
				} else {
					PrintWarn("detach failed (%v); falling back to foreground", err)
				}
			}

			// Foreground.
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			sl := slug.For(wtPath, "")
			repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
			wtID, _ := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, "")
			if len(cfg.Hooks.Predelete) > 0 {
				// Await predelete groups so external cleanup
				// (kill watchers, drop sibling DBs) finishes
				// before TeardownDatabases blows away SQL state.
				started := hooks.EmitHookStart(ctx, st, repoID, wtID, "predelete", len(cfg.Hooks.Predelete))
				out, _ := hooks.RunHooks(ctx, "predelete", cfg.Hooks.Predelete, repoRoot, wtPath, sl.Value, env, true)
				hooks.PersistOutcome(ctx, st, repoID, wtID, "predelete", started, time.Now().UnixMilli(), out)
			}
			_ = prepare.TeardownDatabases(ctx, &cfg, sl.Value, repoID, wtID, st)
			// Mark deleted BEFORE `git worktree remove`. The remove
			// fires the fsnotify REMOVE event the daemon's lifecycle
			// watcher subscribes to; if the row isn't already marked
			// deleted by the time onRemove looks it up, the watcher
			// spawns a duplicate teardownOrphan that re-runs
			// postdelete + DB drop. Marking deleted first closes the
			// window — onRemove's deleted-check short-circuits. If
			// the git remove subsequently fails, the row is left
			// falsely marked deleted; a re-run of `treeman wt delete`
			// resurrects it via EnsureWorktree (deleted_at cleared)
			// before retrying.
			_ = st.MarkWorktreeDeleted(ctx, wtID)
			args := []string{"worktree", "remove"}
			if c.Bool("force") {
				args = append(args, "--force")
			}
			args = append(args, wtPath)
			if err := gitcmd.RunPiped(ctx, repoRoot, os.Stdout, os.Stderr, args...); err != nil && !c.Bool("force") {
				return fmt.Errorf("git worktree remove: %w", err)
			}
			pruneEmptyParents(wtPath, resolveWorktreesRoot(cfg, repoRoot))
			PrintOK("deleted worktree #%d (%s)", wtID, wtPath)
			return nil
		},
	}
}

// pruneEmptyParents walks up from `start` removing now-empty
// directories until we leave `wtRoot` (the configured worktrees
// root). Prevents an empty `.worktrees/feature/` from lingering
// after deleting `.worktrees/feature/foo`. Best-effort: any rmdir
// error stops the walk (e.g. parent is not empty, or permissions).
func pruneEmptyParents(start, wtRoot string) {
	if start == "" || wtRoot == "" {
		return
	}
	parent := filepath.Dir(start)
	for {
		// Stop once we reach (or pass) the worktrees root itself.
		rel, err := filepath.Rel(wtRoot, parent)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return
		}
		if err := os.Remove(parent); err != nil {
			return // non-empty or unreadable — done.
		}
		parent = filepath.Dir(parent)
	}
}

func wtRegister() *cli.Command {
	return &cli.Command{
		Name:  "register",
		Usage: "register a worktree path (metadata only)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "branch", Aliases: []string{"b"}},
			&cli.StringFlag{Name: "repo"},
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
			sl := slug.For(path, branch)
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
			if err != nil {
				return err
			}
			wtID, err := st.EnsureWorktree(ctx, repoID, path, sl.Value, branch)
			if err != nil {
				return err
			}
			fmt.Printf("worktree #%d slug=%s repo=#%d (%s)\n", wtID, sl.Value, repoID, repoRoot)
			return nil
		},
	}
}

func wtUnregister() *cli.Command {
	return &cli.Command{
		Name:  "unregister",
		Usage: "mark a worktree deleted in SQLite without touching git",
		Action: func(ctx context.Context, c *cli.Command) error {
			path := "."
			if c.NArg() >= 1 {
				path = c.Args().First()
			}
			path = MustAbs(path)
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			row := st.DB.QueryRowContext(ctx, "SELECT id FROM worktrees WHERE path = ?", path)
			var id int64
			if err := row.Scan(&id); err != nil {
				return fmt.Errorf("worktree not found: %s", path)
			}
			return st.MarkWorktreeDeleted(ctx, id)
		},
	}
}

func wtList() *cli.Command {
	return &cli.Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Usage:   "list active worktrees",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json"},
			&cli.BoolFlag{Name: "with-state", Usage: "include a STATE column derived from the most recent finalize event"},
			&cli.BoolFlag{Name: "with-status", Usage: "include a STATUS column (clean/dirty/unpushed; forks git status + rev-list per row)"},
			&cli.StringFlag{Name: "sort", Value: "id", Usage: "id | mtime (HEAD commit ts) | visited (last_visited_at)"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "scope to one repo (path)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer st.Close()

			var (
				args    []any
				where   = "deleted_at IS NULL"
				orderBy string
			)
			if r := c.String("repo"); r != "" {
				repoID, _ := st.LookupRepoID(ctx, MustAbs(r))
				if repoID == 0 {
					return fmt.Errorf("no repo registered at %s", r)
				}
				where += " AND repo_id = ?"
				args = append(args, repoID)
			}
			switch c.String("sort") {
			case "visited":
				orderBy = "ORDER BY IFNULL(last_visited_at, 0) DESC, id"
			case "mtime", "id", "":
				orderBy = "ORDER BY id"
			default:
				return fmt.Errorf("unknown --sort %q (id|mtime|visited)", c.String("sort"))
			}
			q := `SELECT id, slug, COALESCE(branch,'-'), path, COALESCE(last_visited_at, 0)
				FROM worktrees WHERE ` + where + ` ` + orderBy
			rows, err := st.DB.QueryContext(ctx, q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			type wtRow struct {
				ID          int64  `json:"id"`
				Slug        string `json:"slug"`
				Branch      string `json:"branch"`
				Path        string `json:"path"`
				HeadTs      int64  `json:"head_ts,omitempty"`
				VisitedTs   int64  `json:"visited_ts,omitempty"`
				Status      string `json:"status,omitempty"`
				StatusError string `json:"status_error,omitempty"`
				Dirty       bool   `json:"dirty,omitempty"`
				Unpushed    bool   `json:"unpushed,omitempty"`
				FinalState  string `json:"state,omitempty"`
			}
			var all []wtRow
			for rows.Next() {
				var r wtRow
				if err := rows.Scan(&r.ID, &r.Slug, &r.Branch, &r.Path, &r.VisitedTs); err != nil {
					return err
				}
				all = append(all, r)
			}
			if err := rows.Err(); err != nil {
				return err
			}

			withStatus := c.Bool("with-status")
			withState := c.Bool("with-state")
			sortMode := c.String("sort")
			needHeadTs := sortMode == "mtime" || c.Bool("json")
			for i := range all {
				r := &all[i]
				if needHeadTs {
					r.HeadTs = headCommitTs(r.Path)
				}
				if withStatus || c.Bool("json") {
					dirty, dErr := worktreeDirty(r.Path)
					unpushed, uErr := gitenv.HasUnpushedCommits(r.Path)
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
				if withState || c.Bool("json") {
					r.FinalState = finalizeStateShort(ctx, st, r.ID)
				}
			}
			if sortMode == "mtime" {
				sort.SliceStable(all, func(i, j int) bool { return all[i].HeadTs > all[j].HeadTs })
			}

			if c.Bool("json") {
				return jsonStream(all)
			}
			if len(all) == 0 {
				ui.Info("no active worktrees")
				ui.Hint("%s", "create one with: treeman wt create <branch>")
				return nil
			}

			headers := []string{"ID", "SLUG", "BRANCH"}
			if withStatus {
				headers = append(headers, "STATUS")
			}
			if withState {
				headers = append(headers, "STATE")
			}
			headers = append(headers, "LAST", "PATH")
			tbl := ui.NewTable(headers...)
			anyStatusErr := false
			for _, r := range all {
				cells := []string{ui.Dim(fmt.Sprintf("%d", r.ID)), ui.Cyan(r.Slug), r.Branch}
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
				cells = append(cells, ui.Dim(lastLabel(r.HeadTs, r.VisitedTs)), r.Path)
				tbl.Row(cells...)
			}
			tbl.Render(nil)
			if anyStatusErr {
				ui.Hint("%s", "STATUS '?' = git status failed; use --json for the per-row error")
			}
			return nil
		},
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
	fmt.Sscanf(out, "%d", &ts)
	return ts
}

// worktreeDirty mirrors gitenv.IsWorktreeClean but inverted: true when
// `git status --porcelain` is non-empty. Returns false + error on
// unreadable git state (treated as not-dirty so a missing repo doesn't
// produce a misleading `*` marker).
func worktreeDirty(path string) (bool, error) {
	clean, err := gitenv.IsWorktreeClean(path)
	if err != nil {
		return false, err
	}
	return !clean, nil
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
		Name:  "finalize",
		Usage: "rerun postcreate + prepare for a worktree (default via daemon; --local runs inline)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo"},
			&cli.BoolFlag{Name: "local", Usage: "run postcreate + prepare in this process instead of dispatching to the daemon"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			path := "."
			if c.NArg() >= 1 {
				path = c.Args().First()
			}
			wtPath := MustAbs(path)
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				repoRoot, err = DiscoverRepoRoot(wtPath)
				if err != nil {
					return err
				}
			}

			if c.Bool("local") {
				// Used by the wt-create fallback path's detached child.
				// Loads its own resolved config + opens its own store
				// handle since the parent has already exited by the
				// time this runs.
				cfg, err := resolve.LoadResolved(repoRoot)
				if err != nil {
					return err
				}
				dbPath, _ := store.DefaultDBPath()
				st, err := store.Open(ctx, dbPath)
				if err != nil {
					return err
				}
				defer st.Close()
				sl := slug.For(wtPath, "")
				repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
				wtID, _ := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, "")
				return runLocalFinalize(ctx, &cfg, repoRoot, wtPath, sl, st, repoID, wtID, CaptureInheritedEnv(), false)
			}

			resp, err := rpc.Call(ctx, rpc.Request{
				Method: rpc.MethodWorktreeFinalize,
				WorktreeFinalize: &rpc.WorktreeFinalizeArgs{
					RepoPath:     repoRoot,
					WorktreePath: wtPath,
					InheritedEnv: CaptureInheritedEnv(),
				},
			})
			if err != nil {
				return err
			}
			if resp.Kind == rpc.KindError {
				return fmt.Errorf("daemon: %s", resp.Message)
			}
			PrintOK("queued: postcreate + prepare detached to daemon — follow with `treeman logs tail --follow`")
			return nil
		},
	}
}

// dispatchTeardown is the wt-delete twin of dispatchFinalize: tries
// the daemon, retries once after ensureDaemon on RPC failure, and
// returns true when the daemon successfully queued the teardown.
func dispatchTeardown(ctx context.Context, repoRoot, wtPath string, force bool, env map[string]string) bool {
	req := rpc.Request{
		Method: rpc.MethodWorktreeTeardown,
		WorktreeTeardown: &rpc.WorktreeTeardownArgs{
			RepoPath:     repoRoot,
			WorktreePath: wtPath,
			Force:        force,
			InheritedEnv: env,
		},
	}
	if resp, err := rpc.Call(ctx, req); err == nil {
		if resp.Kind == rpc.KindWorktreeTeardownQueued {
			PrintOK("queued: predelete + DB teardown + git remove detached to daemon — follow with `treeman logs tail --follow`")
			return true
		}
		if resp.Kind == rpc.KindError {
			PrintWarn("daemon: %s", resp.Message)
		}
	} else {
		PrintWarn("daemon RPC failed (%v); trying daemon restart", err)
		if startErr := ensureDaemon(ctx); startErr == nil {
			if resp, err := rpc.Call(ctx, req); err == nil && resp.Kind == rpc.KindWorktreeTeardownQueued {
				PrintOK("queued: predelete + DB teardown + git remove detached to daemon (auto-restarted)")
				return true
			}
		}
	}
	return false
}

// runLocalFinalize executes the postcreate + prepare tail in the
// current process. Shared by wt create's last-resort foreground
// fallback and by `wt finalize --local` (the subcommand the detach
// helper spawns when the daemon is unreachable).
func runLocalFinalize(
	ctx context.Context,
	cfg *config.Config,
	repoRoot, wtPath string,
	sl slug.Slug,
	st *store.Store,
	repoID, wtID int64,
	env map[string]string,
	skipPrepare bool,
) error {
	if len(cfg.Hooks.Postcreate) > 0 {
		// Block on postcreate completion before prepare so e.g.
		// `composer install` finishes populating vendor/ before
		// artisan migrate runs. Same rationale as the daemon's
		// FinalizeWorktree path.
		started := hooks.EmitHookStart(ctx, st, repoID, wtID, "postcreate", len(cfg.Hooks.Postcreate))
		out, err := hooks.RunHooks(ctx, "postcreate", cfg.Hooks.Postcreate, repoRoot, wtPath, sl.Value, env, true)
		hooks.PersistOutcome(ctx, st, repoID, wtID, "postcreate", started, time.Now().UnixMilli(), out)
		if err != nil {
			return err
		}
		fmt.Printf("postcreate: %d group(s) complete (logs in %s/.treeman-hooks/)\n",
			len(cfg.Hooks.Postcreate), wtPath)
	}
	if skipPrepare || len(cfg.Databases) == 0 {
		return nil
	}
	outs, err := prepare.Run(ctx, cfg, wtPath, sl, st, repoID, wtID, env)
	if err != nil {
		PrintWarn("prepare failed: %v", err)
	}
	for _, o := range outs {
		fmt.Printf("prepare[%s] %s template=%s clones=%d\n",
			o.Engine, o.SourceDB, o.TemplateName, len(o.Clones))
	}
	return nil
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

func resolveWorktreesRoot(cfg config.Config, repoRoot string) string {
	raw := cfg.Worktrees.Root
	if raw == "" {
		raw = ".worktrees"
	}
	if filepath.IsAbs(raw) {
		return raw
	}
	return filepath.Join(repoRoot, raw)
}

func detectDefaultBranch(repoRoot string) string {
	if s, err := gitcmd.String(context.Background(), repoRoot, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if len(s) > len("origin/") && s[:len("origin/")] == "origin/" {
			return s[len("origin/"):]
		}
		return s
	}
	if s, err := gitcmd.String(context.Background(), repoRoot, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		return s
	}
	return "main"
}

// wtSwitch — `treeman wt switch [name]`. Prints the absolute path
// of an existing worktree to stdout (so a shell shim can `cd
// "$(treeman wt switch foo)"`). With `--create`, runs the full
// create flow first when no match is found.
//
// Lookup order:
//  1. SQLite active worktrees: exact basename match, exact slug
//     match, then prefix match on either.
//  2. Filesystem: `<worktrees-root>/<name>` exists.
//
// On ambiguous prefix matches, lists candidates on stderr and exits
// non-zero. Status/warnings go to stderr; only the resolved path
// goes to stdout — never mix the two streams.
func wtSwitch() *cli.Command {
	return &cli.Command{
		Name:      "switch",
		Usage:     "print the path of a worktree (for shell `cd $(…)` use)",
		ArgsUsage: "<name>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Usage: "repo root override"},
			&cli.BoolFlag{Name: "create", Usage: "create the worktree if no match", Value: false},
			&cli.StringFlag{Name: "from", Usage: "base branch (with --create)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt switch <name> [--create]")
			}
			name := c.Args().First()
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			cfg, err := resolve.LoadResolved(repoRoot)
			if err != nil {
				return err
			}

			if path, ok := lookupWorktree(ctx, repoRoot, name); ok {
				touchVisitedByPath(ctx, path)
				fmt.Println(path)
				return nil
			}

			// Filesystem fallback.
			fsCandidate := filepath.Join(resolveWorktreesRoot(cfg, repoRoot), name)
			if fi, err := os.Stat(fsCandidate); err == nil && fi.IsDir() {
				fmt.Println(fsCandidate)
				return nil
			}

			if !c.Bool("create") {
				return fmt.Errorf("no worktree matches %q (try `treeman wt switch %s --create`)", name, name)
			}

			// Delegate to the create flow, then print the resolved
			// path. Reuse wtCreate's Action via a synthetic args
			// vector keeps the codepath single-source.
			createCmd := wtCreate()
			argv := []string{"create", name}
			if v := c.String("from"); v != "" {
				argv = append(argv, "--from", v)
			}
			argv = append(argv, "--repo", repoRoot)
			if err := createCmd.Run(ctx, argv); err != nil {
				return err
			}
			// After create, wtPath is `<root>/<name>` by default.
			fmt.Println(filepath.Join(resolveWorktreesRoot(cfg, repoRoot), name))
			return nil
		},
	}
}

// lookupWorktree queries SQLite for active worktrees attached to
// `repoRoot` and returns the path that best matches `name` (exact
// basename, then exact slug, then unambiguous prefix).
func lookupWorktree(ctx context.Context, repoRoot, name string) (string, bool) {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return "", false
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return "", false
	}
	defer st.Close()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT w.path, w.slug, COALESCE(w.branch,'')
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE w.deleted_at IS NULL AND r.path = ?`, repoRoot)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	var exactBase, exactSlug, exactBranch string
	var prefixHits []string
	for rows.Next() {
		var path, slug, branch string
		if err := rows.Scan(&path, &slug, &branch); err != nil {
			continue
		}
		base := filepath.Base(path)
		switch {
		case base == name:
			exactBase = path
		case slug == name:
			exactSlug = path
		case branch == name:
			exactBranch = path
		case len(name) >= 2 && (hasPrefix(base, name) || hasPrefix(slug, name) || hasPrefix(branch, name)):
			prefixHits = append(prefixHits, path)
		}
	}
	switch {
	case exactBase != "":
		return exactBase, true
	case exactBranch != "":
		return exactBranch, true
	case exactSlug != "":
		return exactSlug, true
	case len(prefixHits) == 1:
		return prefixHits[0], true
	case len(prefixHits) > 1:
		fmt.Fprintln(os.Stderr, "ambiguous prefix match — candidates:")
		for _, p := range prefixHits {
			fmt.Fprintln(os.Stderr, "  ", p)
		}
		return "", false
	}
	return "", false
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// wtBack — `treeman wt back`. Prints the main repo root for the
// current cwd. With `--remove`, also deletes the current linked
// worktree if it is clean (`git status --porcelain` empty) and has
// no commits ahead of upstream.
//
// Used by the zsh shim: `cd "$(treeman wt back)"`. The remove path
// shells out to `treeman wt delete` so the standard teardown +
// daemon RPC flow runs.
func wtBack() *cli.Command {
	return &cli.Command{
		Name:  "back",
		Usage: "print main repo path (with --remove, drop current worktree if clean)",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "remove", Usage: "delete current worktree if clean + no unpushed commits"},
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

			clean, err := gitenv.IsWorktreeClean(wtRoot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "git status failed: %v; --remove aborted\n", err)
				fmt.Println(repoRoot)
				return nil
			}
			if !clean && !c.Bool("force") {
				fmt.Fprintln(os.Stderr, "worktree has uncommitted changes; refusing --remove (pass --force to override)")
				fmt.Println(repoRoot)
				return nil
			}
			unpushed, _ := gitenv.HasUnpushedCommits(wtRoot)
			if unpushed && !c.Bool("force") {
				fmt.Fprintln(os.Stderr, "worktree has commits ahead of upstream; refusing --remove (pass --force to override)")
				fmt.Println(repoRoot)
				return nil
			}

			// Print main repo path FIRST so the caller can `cd "$(…)"`
			// even when the subsequent delete prints to stderr.
			fmt.Println(repoRoot)

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
			if err := wtDelete().Run(ctx, argv); err != nil {
				fmt.Fprintf(os.Stderr, "wt delete failed: %v\n", err)
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

// wtResolve — `treeman wt resolve <branch>`. Pure registry lookup.
// Prints the absolute worktree path for the given branch (exact
// match on `worktrees.branch`) and returns nonzero with a clear
// stderr message when no live worktree owns that branch. Used by
// shell shims to replace `_worktree_path_for_branch` (which forks
// `git worktree list --porcelain` per call).
func wtResolve() *cli.Command {
	return &cli.Command{
		Name:      "resolve",
		Usage:     "print the worktree path holding <branch> (registry lookup; exit nonzero on miss)",
		ArgsUsage: "<branch>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt resolve <branch>")
			}
			branch := c.Args().First()
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			path, ok := registryWorktreeForBranch(ctx, repoRoot, branch)
			if !ok {
				return fmt.Errorf("no live worktree on branch %q in %s", branch, repoRoot)
			}
			fmt.Println(path)
			return nil
		},
	}
}

// wtPrev — `treeman wt prev`. Prints the path of the previously-
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
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			repoID, _ := st.LookupRepoID(ctx, repoRoot)
			if repoID == 0 {
				return fmt.Errorf("repo %s not registered yet", repoRoot)
			}
			cwd, _ := os.Getwd()
			cwdTop, _ := gitWorktreeRoot(cwd)
			path, ok := st.PrevVisitedWorktree(ctx, repoID, cwdTop)
			if !ok {
				return fmt.Errorf("no previously-visited worktree on record for %s", repoRoot)
			}
			touchVisitedByPath(ctx, path)
			fmt.Println(path)
			return nil
		},
	}
}

// wtGo — `treeman wt go <branch> [--create] [--from <base>]`. Full
// checkout/create-routing: exposes the policy zsh's
// `_checkout_branch_or_worktree` implements with N git forks.
//
// Routing:
//  1. Branch already live in another worktree → print that path.
//  2. cwd is in a linked worktree AND main repo is clean → run
//     git checkout (or -b) in main, print main.
//  3. cwd is in a linked worktree AND main is dirty → delegate to
//     `wt create` so the new branch lands in a fresh worktree,
//     print the new worktree path.
//  4. Else (we're in main or a standalone clone) → run git
//     checkout in cwd's repo root, print that path.
//
// Always prints exactly one line on stdout (the destination
// directory). Status / warnings go to stderr. Touches
// last_visited_at on every successful resolution so `wt prev`
// works.
func wtGo() *cli.Command {
	return &cli.Command{
		Name:      "go",
		Usage:     "switch/create branch with auto-routing (use as cd \"$(treeman wt go …)\")",
		ArgsUsage: "<branch>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "create", Usage: "treat <branch> as a new branch (falls back to checkout if it already exists)"},
			&cli.StringFlag{Name: "from", Usage: "base branch (with --create)"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "no-fetch", Usage: "skip the pre-checkout `git fetch origin <base>`"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt go <branch> [--create]")
			}
			branch := c.Args().First()
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}

			// (1) Already live in some worktree?
			if path, ok := registryWorktreeForBranch(ctx, repoRoot, branch); ok {
				cwd, _ := os.Getwd()
				cwdTop, _ := gitWorktreeRoot(cwd)
				if path != cwdTop {
					touchVisitedByPath(ctx, path)
					fmt.Println(path)
					return nil
				}
			}

			// Mode resolution (create-but-exists → checkout).
			mode := "checkout"
			base := c.String("from")
			if c.Bool("create") {
				mode = "create"
				if refExistsLocal(repoRoot, branch) {
					mode = "checkout"
					base = ""
				}
			}

			// Optional pre-fetch so a stale local base doesn't seed
			// the new branch off yesterday's commit. Same heuristic
			// as zsh's _resolve_base.
			if mode == "create" && base != "" && !c.Bool("no-fetch") {
				_ = gitcmd.RunPiped(ctx, repoRoot, nil, nil, "fetch", "origin", base, "--quiet")
				if refExistsRemote(repoRoot, base) {
					base = "origin/" + base
				}
			}

			cwd, _ := os.Getwd()
			cwdTop, _ := gitWorktreeRoot(cwd)
			inLinked := cwdTop != "" && gitenv.IsLinkedWorktree(cwdTop)

			runCheckoutIn := func(dir string) error {
				args := []string{"checkout"}
				if mode == "create" {
					args = append(args, "-b", branch)
					if base != "" {
						args = append(args, base)
					}
				} else {
					args = append(args, branch)
				}
				// stdout goes to stderr — `wt go` reserves stdout for
				// the worktree path (consumed by `cd $(treeman wt go …)`).
				return gitcmd.RunPiped(ctx, dir, os.Stderr, os.Stderr, args...)
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
			mainClean, _ := gitenv.IsWorktreeClean(repoRoot)
			if mainClean {
				if err := runCheckoutIn(repoRoot); err != nil {
					return err
				}
				touchVisitedByPath(ctx, repoRoot)
				fmt.Println(repoRoot)
				return nil
			}

			// (3) main dirty → spawn a fresh worktree.
			argv := []string{"create", branch, "--repo", repoRoot}
			if c.String("from") != "" {
				argv = append(argv, "--from", c.String("from"))
			}
			if err := wtCreate().Run(ctx, argv); err != nil {
				return err
			}
			// Resolve final path from registry (wt create writes the
			// row before returning).
			if path, ok := registryWorktreeForBranch(ctx, repoRoot, branch); ok {
				touchVisitedByPath(ctx, path)
				fmt.Println(path)
				return nil
			}
			// Fall back to the default location (matches wt create's path math).
			cfg, _ := resolve.LoadResolved(repoRoot)
			path := filepath.Join(resolveWorktreesRoot(cfg, repoRoot), branch)
			fmt.Println(path)
			return nil
		},
	}
}

// registryWorktreeForBranch finds the live worktree on <branch> for
// <repoRoot>. Returns ("", false) when no row matches. Branch is an
// exact match on `worktrees.branch` — the column is populated by
// `wt create` / `wt register --branch`.
func registryWorktreeForBranch(ctx context.Context, repoRoot, branch string) (string, bool) {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return "", false
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return "", false
	}
	defer st.Close()
	row := st.DB.QueryRowContext(ctx, `
		SELECT w.path FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? AND w.deleted_at IS NULL AND w.branch = ?
		ORDER BY w.id DESC LIMIT 1`, repoRoot, branch)
	var p string
	if err := row.Scan(&p); err != nil {
		return "", false
	}
	return p, true
}

// isMatchingExistingWorktree reports whether `wtPath` is already a
// linked git worktree of `repoRoot` checked out on `branch` and is
// present in the SQLite registry. Used by `wt create` to short-
// circuit re-creation: a script that retries the command after a
// transient failure shouldn't error with "path already exists" when
// the previous run actually succeeded.
//
// All four conditions must hold: (1) `wtPath` exists, (2) its
// `.git` file is a gitlink (not a directory — so it's a linked
// worktree), (3) `git -C <wtPath> branch --show-current` equals
// `branch`, (4) the registry has an active row for this path.
// Mismatches return false → caller falls back to the original
// "already exists" error so we never silently swallow drift.
func isMatchingExistingWorktree(ctx context.Context, repoRoot, wtPath, branch string) bool {
	if !gitenv.IsLinkedWorktree(wtPath) {
		return false
	}
	current, err := gitcmd.String(ctx, wtPath, "branch", "--show-current")
	if err != nil || current != branch {
		return false
	}
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return false
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return false
	}
	defer st.Close()
	var n int
	_ = st.DB.QueryRowContext(ctx,
		`SELECT 1 FROM worktrees w JOIN repos r ON r.id = w.repo_id
		 WHERE r.path = ? AND w.path = ? AND w.deleted_at IS NULL`,
		repoRoot, wtPath).Scan(&n)
	return n == 1
}

// refExistsLocal returns true when refs/heads/<name> resolves in
// repoRoot. Used by wt go to flip --create → checkout when the
// branch already exists.
func refExistsLocal(repoRoot, name string) bool {
	return gitcmd.Exists(context.Background(), repoRoot, "refs/heads/"+name)
}

func refExistsRemote(repoRoot, name string) bool {
	return gitcmd.Exists(context.Background(), repoRoot, "refs/remotes/origin/"+name)
}

// touchVisitedByPath stamps last_visited_at on the worktree row at
// `path`. Swallows errors — visit-tracking is best-effort metadata,
// not a correctness gate.
func touchVisitedByPath(ctx context.Context, path string) {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return
	}
	defer st.Close()
	_ = st.TouchWorktreeVisitedByPath(ctx, path)
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// bringInFiles brings each entry in `paths` from repoRoot into wtPath
// via either symlink (mode="link") or recursive copy (mode="copy").
// Glob meta-characters (`*?[`) expand against repoRoot. Idempotent —
// if the destination already exists the entry is skipped.
func bringInFiles(repoRoot, wtPath string, paths []string, mode string) error {
	for _, rel := range paths {
		var matches []string
		if strings.ContainsAny(rel, "*?[") {
			m, _ := filepath.Glob(filepath.Join(repoRoot, rel))
			matches = m
		} else {
			matches = []string{filepath.Join(repoRoot, rel)}
		}
		if len(matches) == 0 {
			continue
		}
		for _, src := range matches {
			info, err := os.Stat(src)
			if err != nil {
				if !strings.ContainsAny(rel, "*?[") {
					PrintWarn("%s source missing, skipping: %s", mode, src)
				}
				continue
			}
			relToRepo, err := filepath.Rel(repoRoot, src)
			if err != nil {
				relToRepo = filepath.Base(src)
			}
			dst := filepath.Join(wtPath, relToRepo)
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
			}
			switch mode {
			case "link":
				if err := os.Symlink(src, dst); err != nil {
					return fmt.Errorf("symlink %s → %s: %w", dst, src, err)
				}
			case "copy":
				if err := copyPath(src, dst, info); err != nil {
					return fmt.Errorf("copy %s → %s: %w", src, dst, err)
				}
			}
		}
	}
	return nil
}

// copyPath copies src → dst. Regular files are copied byte-for-byte
// with the source's mode preserved; directories are recursed; symlinks
// in the source tree are recreated as symlinks pointing at the same
// target (so a `.env` symlink in main becomes a `.env` symlink in the
// worktree, not a copy of the symlinked file).
func copyPath(src, dst string, info os.FileInfo) error {
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case mode.IsDir():
		if err := os.MkdirAll(dst, mode.Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childSrc := filepath.Join(src, e.Name())
			childDst := filepath.Join(dst, e.Name())
			childInfo, err := os.Lstat(childSrc)
			if err != nil {
				return err
			}
			if err := copyPath(childSrc, childDst, childInfo); err != nil {
				return err
			}
		}
		return nil
	case mode.IsRegular():
		return copyRegularFile(src, dst, mode.Perm())
	default:
		return fmt.Errorf("unsupported file type for %s (mode=%v)", src, mode)
	}
}

func copyRegularFile(src, dst string, perm os.FileMode) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer df.Close()
	if _, err := io.Copy(df, sf); err != nil {
		return err
	}
	return nil
}
