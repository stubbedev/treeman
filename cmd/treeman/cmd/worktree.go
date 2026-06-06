package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/wt"
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
			wtBack(),
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
in SQLite, then dispatches setup hooks + prepare to the daemon. The
CLI always returns immediately — follow progress with
'treeman logs tail --follow'.

Examples:
  treeman wt create PROJ-1234
  treeman wt create feature/x --from origin/develop
  cd "$(treeman wt create feat --print-path)"`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "base branch"},
			&cli.StringFlag{Name: "path", Usage: "explicit worktree path"},
			&cli.StringFlag{Name: "repo", Usage: "repo root override"},
			&cli.BoolFlag{Name: "skip-hooks"},
			&cli.BoolFlag{Name: "skip-prepare"},
			&cli.BoolFlag{
				Name:  "no-fetch",
				Usage: "skip the pre-create `git fetch origin <base>` (defaults on so new branches pick up upstream commits)",
			},
			&cli.BoolFlag{
				Name:  "print-path",
				Usage: "print only the new worktree path on stdout; status lines redirect to stderr (enables `cd \"$(treeman wt create …)\"`)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman wt create <branch>")
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
			task := rpc.Task{
				Type:         rpc.TaskWorktreeCreate,
				RepoPath:     repoRoot,
				Params:       map[string]string{"branch": branch},
				InheritedEnv: CaptureInheritedEnv(),
			}
			if v := c.String("from"); v != "" {
				task.Params["from"] = v
			}
			if v := c.String("path"); v != "" {
				task.Params["path"] = v
			}
			if c.Bool("no-fetch") {
				task.Params["no_fetch"] = "1"
			}
			if c.Bool("skip-hooks") {
				task.Params["skip_hooks"] = "1"
			}
			if c.Bool("skip-prepare") {
				task.Params["skip_prepare"] = "1"
			}
			payload, err := resultPayload(ctx, task)
			if err != nil {
				return err
			}
			var res wt.CreateResult
			if err := json.Unmarshal(payload, &res); err != nil {
				return err
			}
			printCreateResult(res)
			if printPathOnly {
				_, _ = fmt.Fprintln(os.Stdout, res.WtPath)
			}
			return nil
		},
	}
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
	default:
		PrintOK("created worktree #%d slug=%s path=%s", res.WorktreeID, res.Slug, res.WtPath)
		PrintOK("queued: setup + prepare on daemon — follow with `treeman logs tail --follow`")
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
		Aliases:   []string{"rm"},
		Usage:     "delete a worktree end-to-end",
		ArgsUsage: "<path-or-branch>",
		Description: `Runs teardown hooks + DB teardown + git worktree remove, then
removes the registry row. The teardown is dispatched to the daemon
(or a setsid child when the daemon is unreachable) — the CLI
always returns immediately.

Examples:
  treeman wt delete PROJ-1234
  treeman wt delete /path/to/wt --force      # remove stale registry entry`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo"},
			&cli.BoolFlag{Name: "force", Aliases: []string{"f"}},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
			// `--detached` is the internal flag set by detachLocalDelete
			// when the daemon RPC is unreachable. Tells this process
			// "you ARE the detached worker; run the teardown inline
			// instead of trying to dispatch back to the daemon".
			// Not documented in help.
			&cli.BoolFlag{Name: "detached", Hidden: true},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman wt delete <path-or-branch>")
			}
			target := c.Args().First()

			// Resolve the repo root first (--repo flag > cwd walk-up >
			// fall back to target-as-path). Needed before the branch /
			// slug lookup so the SQLite query is scoped correctly.
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				repoRoot, err = DiscoverRepoRoot(MustAbs(target))
				if err != nil {
					return err
				}
			}

			// Confirmation lives in the CLI — wt.Delete itself is
			// non-interactive. Resolve the wtPath up front so the
			// prompt can name the target precisely; this duplicates
			// wt.Delete's lookup but the second pass inside the
			// orchestrator is cheap (sqlite query).
			wtPath := MustAbs(target)
			if p, ok := wt.LookupWorktree(ctx, repoRoot, target, cliSink{}); ok {
				wtPath = p
			}

			if !c.Bool("yes") {
				q := fmt.Sprintf("delete worktree %s and drop its databases?", wtPath)
				if !ui.Confirm(q) {
					PrintInfo("aborted: worktree %s left intact", wtPath)
					return nil
				}
			}

			_, err = wt.Delete(ctx, wt.DeleteRequest{
				RepoRoot: repoRoot,
				Target:   target,
				Force:    c.Bool("force"),
				Env:      CaptureInheritedEnv(),
				Detached: c.Bool("detached"),
			}, cliSink{})
			return err
		},
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
			payload, err := resultPayload(ctx, rpc.Task{
				Type: rpc.TaskWtRegister, RepoPath: repoRoot, WorktreePath: path,
				Params: map[string]string{"branch": branch},
			})
			if err != nil {
				return err
			}
			var r struct {
				WorktreeID int64  `json:"worktree_id"`
				RepoID     int64  `json:"repo_id"`
				Slug       string `json:"slug"`
			}
			_ = json.Unmarshal(payload, &r)
			fmt.Printf("worktree #%d slug=%s repo=#%d (%s)\n", r.WorktreeID, r.Slug, r.RepoID, repoRoot)
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
			_, err := resultPayload(ctx, rpc.Task{Type: rpc.TaskWtUnregister, WorktreePath: path})
			return err
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
			&cli.BoolFlag{
				Name:  "with-status",
				Usage: "include a STATUS column (clean/dirty/unpushed; forks git status + rev-list per row)",
			},
			&cli.StringFlag{Name: "sort", Value: "id", Usage: "id | mtime (HEAD commit ts) | visited (last_visited_at)"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "scope to one repo (path)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

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
			//nolint:gosec // where/orderBy are fixed fragments; values are parameterized via args
			q := `SELECT id, slug, COALESCE(branch,'-'), path, COALESCE(last_visited_at, 0), is_main
				FROM worktrees WHERE ` + where + ` ` + orderBy
			rows, err := st.DB.QueryContext(ctx, q, args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			var all []wtRow
			anyMain := false
			for rows.Next() {
				var r wtRow
				if err := rows.Scan(&r.ID, &r.Slug, &r.Branch, &r.Path, &r.VisitedTs, &r.IsMain); err != nil {
					return err
				}
				if r.IsMain {
					anyMain = true
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
			enrichWtRows(ctx, st, all, withStatus, withState, needHeadTs, c.Bool("json"))
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

			renderWtTable(all, anyMain, withStatus, withState)
			return nil
		},
	}
}

// wtRow is one row of `treeman wt list` output.
type wtRow struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	IsMain      bool   `json:"is_main"`
	HeadTs      int64  `json:"head_ts,omitempty"`
	VisitedTs   int64  `json:"visited_ts,omitempty"`
	Status      string `json:"status,omitempty"`
	StatusError string `json:"status_error,omitempty"`
	Dirty       bool   `json:"dirty,omitempty"`
	Unpushed    bool   `json:"unpushed,omitempty"`
	FinalState  string `json:"state,omitempty"`
}

// enrichWtRows fills in per-row HEAD timestamp, git status and finalize
// state for each worktree, honoring which optional columns were
// requested (or JSON output, which always populates everything).
func enrichWtRows(ctx context.Context, st *store.Store, all []wtRow, withStatus, withState, needHeadTs, asJSON bool) {
	for i := range all {
		r := &all[i]
		if needHeadTs {
			r.HeadTs = headCommitTs(r.Path)
		}
		if withStatus || asJSON {
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
		if withState || asJSON {
			r.FinalState = finalizeStateShort(ctx, st, r.ID)
		}
	}
}

// renderWtTable prints the human-readable `treeman wt list` table.
func renderWtTable(all []wtRow, anyMain, withStatus, withState bool) {
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
		cells = append(cells, ui.Dim(lastLabel(r.HeadTs, r.VisitedTs)), r.Path)
		tbl.Row(cells...)
	}
	tbl.Render(nil)
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

// worktreeDirty mirrors gitenv.IsWorktreeClean but inverted: true when
// `git status --porcelain` is non-empty. Returns false + error on
// unreadable git state (treated as not-dirty so a missing repo doesn't
// produce a misleading `*` marker).
func worktreeDirty(ctx context.Context, path string) (bool, error) {
	clean, err := gitenv.IsWorktreeClean(ctx, path)
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
		Name:      "finalize",
		Usage:     "rerun setup + prepare for a worktree (default via daemon; --local runs inline)",
		ArgsUsage: "[path|slug|branch]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo"},
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
			p, ok := wt.LookupWorktree(ctx, repoRoot, arg, cliSink{})
			if !ok {
				return "", "", fmt.Errorf("no worktree matches %q (expected a path or registered slug/branch)", arg)
			}
			wtPath = p
		}
	}
	return wtPath, repoRoot, nil
}

// runLocalFinalizeFlow runs setup + prepare inline (used by the
// wt-create fallback path's detached child). Loads its own resolved
// config + store handle since the parent has already exited by the time
// this runs. Routes through wt.ResolveIdentity so `wt finalize . --local`
// at the repo root picks up the main-wt overlay/slug instead of producing
// a path-hash slug that would corrupt the main row.
func runLocalFinalizeFlow(ctx context.Context, repoRoot, wtPath string) error {
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return err
	}
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
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

			clean, err := gitenv.IsWorktreeClean(ctx, wtRoot)
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
			unpushed, _ := gitenv.HasUnpushedCommits(ctx, wtRoot)
			if unpushed && !c.Bool("force") {
				fmt.Fprintln(os.Stderr, "worktree has commits ahead of upstream; refusing --remove (pass --force to override)")
				fmt.Println(repoRoot)
				return nil
			}

			// Print main repo path FIRST so the caller can `cd "$(…)"`;
			// stdout must carry only the path. The delete below routes
			// its status through ui.Out, so redirect ui.Out to stderr
			// for that call to keep stdout path-only.
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
			prevOut := ui.Out
			ui.Out = os.Stderr
			err = wtDelete().Run(ctx, argv)
			ui.Out = prevOut
			if err != nil {
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
			defer func() { _ = st.Close() }()
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

// wtGo — `treeman wt go <name-or-branch>`. The single navigation verb;
// absorbs the former `wt switch` and `wt resolve`.
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
		Usage:     "resolve/create/checkout a worktree by name or branch (use as cd \"$(treeman wt go …)\")",
		ArgsUsage: "<name-or-branch>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "create", Usage: "create the worktree if nothing matches"},
			&cli.BoolFlag{
				Name:  "checkout",
				Usage: "git checkout the branch (auto-routes main vs new worktree) instead of pure path resolution",
			},
			&cli.StringFlag{Name: "from", Usage: "base branch (with --create/--checkout)"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "no-fetch", Usage: "skip the pre-checkout `git fetch origin <base>`"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman wt go <name-or-branch>")
			}
			target := c.Args().First()
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}

			if c.Bool("checkout") {
				return goCheckout(ctx, repoRoot, target, c.String("from"), c.Bool("create"), c.Bool("no-fetch"))
			}

			// Pure resolve: fuzzy worktree lookup, then exact branch.
			if path, ok := wt.LookupWorktree(ctx, repoRoot, target, cliSink{}); ok {
				touchVisitedByPath(ctx, path)
				fmt.Println(path)
				return nil
			}
			if path, ok := registryWorktreeForBranch(ctx, repoRoot, target); ok {
				touchVisitedByPath(ctx, path)
				fmt.Println(path)
				return nil
			}

			cfg, err := resolve.LoadResolved(repoRoot)
			if err != nil {
				return err
			}
			fsCandidate := filepath.Join(wt.WorktreesRoot(cfg, repoRoot), target)
			if fi, statErr := os.Stat(fsCandidate); statErr == nil && fi.IsDir() {
				fmt.Println(fsCandidate)
				return nil
			}

			if !c.Bool("create") {
				return fmt.Errorf(
					"no worktree matches %q (try `treeman wt go %s --create`, or --checkout to switch branches)",
					target,
					target,
				)
			}

			// --create → spawn the worktree, then print its path.
			createCmd := wtCreate()
			argv := []string{"create", target}
			if v := c.String("from"); v != "" {
				argv = append(argv, "--from", v)
			}
			argv = append(argv, "--repo", repoRoot)
			// Silence create's status lines on stdout — wt go reserves
			// stdout for the resolved path (cd "$(treeman wt go …)").
			prevOut := ui.Out
			ui.Out = os.Stderr
			err = createCmd.Run(ctx, argv)
			ui.Out = prevOut
			if err != nil {
				return err
			}
			fmt.Println(filepath.Join(wt.WorktreesRoot(cfg, repoRoot), target))
			return nil
		},
	}
}

// goCheckout runs the branch-checkout auto-routing for `wt go --checkout`.
// Prints exactly one line (the destination directory) on stdout; status
// goes to stderr.
func goCheckout(ctx context.Context, repoRoot, branch, from string, create, noFetch bool) error {
	// (1) Already live in some worktree (other than cwd)?
	if path, ok := registryWorktreeForBranch(ctx, repoRoot, branch); ok {
		cwd, _ := os.Getwd()
		cwdTop, _ := gitWorktreeRoot(cwd)
		if path != cwdTop {
			touchVisitedByPath(ctx, path)
			fmt.Println(path)
			return nil
		}
	}

	mode, base := resolveGoMode(ctx, repoRoot, branch, from, create, noFetch)

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
	return goSpawnWorktree(ctx, repoRoot, branch, from)
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

// goSpawnWorktree creates a fresh worktree for <branch> (the main-dirty
// path) and prints its resolved path on stdout.
func goSpawnWorktree(ctx context.Context, repoRoot, branch, from string) error {
	argv := []string{"create", branch, "--repo", repoRoot}
	if from != "" {
		argv = append(argv, "--from", from)
	}
	// Silence create's status lines on stdout — wt go reserves stdout
	// for the resolved path (cd "$(treeman wt go …)").
	prevOut := ui.Out
	ui.Out = os.Stderr
	err := wtCreate().Run(ctx, argv)
	ui.Out = prevOut
	if err != nil {
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
	path := filepath.Join(wt.WorktreesRoot(cfg, repoRoot), branch)
	fmt.Println(path)
	return nil
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
	defer func() { _ = st.Close() }()
	row := st.DB.QueryRowContext(ctx, `
		SELECT w.path FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? COLLATE NOCASE AND w.deleted_at IS NULL AND w.branch = ?
		ORDER BY w.id DESC LIMIT 1`, repoRoot, branch)
	var p string
	if err := row.Scan(&p); err != nil {
		return "", false
	}
	return p, true
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
	defer func() { _ = st.Close() }()
	_ = st.TouchWorktreeVisitedByPath(ctx, path)
}
