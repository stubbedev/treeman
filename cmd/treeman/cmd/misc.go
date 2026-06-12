package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/daemonctl"
	"github.com/stubbedev/treeman/internal/initgen"
	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/schema"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
	wt2 "github.com/stubbedev/treeman/internal/wt"
)

// PrepareCmd — `treeman prepare` runs the full pipeline foreground.
func PrepareCmd() *cli.Command {
	return &cli.Command{
		Name:  "prepare",
		Usage: "ensure → dump → migrate → snapshot → replicate (foreground)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "worktree", Aliases: []string{"w"}},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "json"},
			&cli.BoolFlag{
				Name:    "foreground",
				Aliases: []string{"wait", "f"},
				Usage:   "stream the daemon's live progress and block until done (default: dispatch and return)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			wtPath, repoRoot, err := resolveWtRepo(c.String("worktree"), c.String("repo"))
			if err != nil {
				return err
			}
			task := rpc.Task{
				Type: rpc.TaskPrepare, RepoPath: repoRoot, WorktreePath: wtPath,
				InheritedEnv: CaptureInheritedEnv(),
			}
			if c.Bool("json") {
				return runResultJSON(ctx, task)
			}
			return dispatchTask(ctx, task, c.Bool("foreground"), "prepare")
		},
	}
}

// The daemon is the preferred mutator of repo/worktree state: the CLI
// builds a plan (parallel groups; tasks within a group run sequentially)
// and dispatches it. When no daemon is reachable the same plan runs
// in-process (submitPlan), so every command works daemon-less.

// dispatchTask submits a single-task plan. When foreground is false it
// queues the task on the daemon and returns at once; when no daemon is
// reachable it runs the task in-process and reports the outcome. When
// foreground is true it streams the daemon's live event tail (subscribing
// before dispatch so no event is missed), or runs in-process when there's
// no daemon. label is the human noun for messages.
func dispatchTask(ctx context.Context, task rpc.Task, foreground bool, label string) error {
	if foreground {
		return foregroundTask(ctx, task, label)
	}
	resp := submitPlan(ctx, rpc.Plan(false, rpc.One(task)))
	switch resp.Kind {
	case rpc.KindPlanQueued:
		PrintOK("queued: %s detached to daemon — follow with `treeman logs tail --follow` (or re-run with --foreground)", label)
		return nil
	case rpc.KindPlanResult:
		return reportInlineResult(resp, label) // ran in-process (no daemon)
	default:
		return fmt.Errorf("%s: %s", label, resp.Message)
	}
}

// foregroundTask streams the daemon's live events to completion, or — when
// no daemon is reachable — runs the plan in-process and reports the outcome.
func foregroundTask(ctx context.Context, task rpc.Task, label string) error {
	runID := runid.New()
	ch, cancel, err := subscribeRun(ctx, runID)
	if err != nil {
		return reportInlineResult(submitPlan(ctx, rpc.Plan(true, rpc.One(task))), label)
	}
	defer cancel()
	req := rpc.Plan(false, rpc.One(task))
	req.RunPlan.RunID = runID
	if resp, callErr := rpc.Call(ctx, req); callErr != nil || resp.Kind != rpc.KindPlanQueued {
		return reportInlineResult(submitPlan(ctx, rpc.Plan(true, rpc.One(task))), label)
	}
	return streamPlanEvents(ch, label)
}

// reportInlineResult prints the outcome of an in-process plan run and
// surfaces the first failed task as an error.
func reportInlineResult(resp rpc.Response, label string) error {
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("%s: %s", label, resp.Message)
	}
	for _, r := range resp.TaskResults {
		if !r.OK {
			return fmt.Errorf("%s: %s", label, r.Message)
		}
	}
	PrintOK("%s complete", label)
	return nil
}

// resultTask runs a single-task plan to completion and returns the
// per-task results (for --json / result-oriented callers). Dispatched to
// the daemon when reachable, otherwise run in-process. A failed task
// surfaces as an error.
func resultTask(ctx context.Context, task rpc.Task) ([]rpc.TaskResult, error) {
	resp := submitPlan(ctx, rpc.Plan(true, rpc.One(task)))
	if resp.Kind == rpc.KindError {
		return nil, errors.New(resp.Message)
	}
	if resp.Kind != rpc.KindPlanResult {
		return nil, fmt.Errorf("unexpected response %q", resp.Kind)
	}
	for _, r := range resp.TaskResults {
		if !r.OK {
			return resp.TaskResults, fmt.Errorf("%s: %s", r.Type, r.Message)
		}
	}
	return resp.TaskResults, nil
}

// resultPayload runs a single-task result plan and returns the task's
// JSON payload bytes, for callers that render a human summary from the
// structured result.
func resultPayload(ctx context.Context, task rpc.Task) ([]byte, error) {
	results, err := resultTask(ctx, task)
	if err != nil || len(results) == 0 {
		return nil, err
	}
	return []byte(results[0].PayloadJSON), nil
}

// runResultJSON runs a single-task result plan and prints the task's
// structured JSON payload to stdout (the --json surface).
func runResultJSON(ctx context.Context, task rpc.Task) error {
	payload, err := resultPayload(ctx, task)
	if err != nil || payload == nil {
		return err
	}
	_, werr := fmt.Fprintln(ui.Out, string(payload))
	return werr
}

// finalizeRequest builds a fire-and-forget run_plan for the worktree-
// finalize tail (setup hooks + prepare) — used by `wt finalize` and
// `main enable`.
func finalizeRequest(repoRoot, wtPath string) rpc.Request {
	return rpc.Plan(false, rpc.One(rpc.Task{
		Type:         rpc.TaskWorktreeFinalize,
		RepoPath:     repoRoot,
		WorktreePath: wtPath,
		InheritedEnv: CaptureInheritedEnv(),
	}))
}

// writeConfig persists a CLI-rendered config body through the daemon —
// the sole writer of .treeman.yaml. The daemon snapshots the previous
// content, atomic-writes the new body, and reloads the repo.
func writeConfig(ctx context.Context, repoRoot, path string, body []byte) error {
	_, err := resultTask(ctx, rpc.Task{
		Type: rpc.TaskConfigWrite, RepoPath: repoRoot,
		Params: map[string]string{rpc.ParamPath: path, rpc.ParamBody: string(body)},
	})
	return err
}

// submitPlan runs a plan: dispatch to the daemon when reachable, else
// execute it in-process. treeman works daemon-less — the daemon is the
// preferred mutator (fast async return, single writer), not a hard
// requirement. Always yields a response: KindPlanQueued from a live
// daemon, KindPlanResult from in-process, or KindError.
func submitPlan(ctx context.Context, req rpc.Request) rpc.Response {
	if resp, err := rpc.Call(ctx, req); err == nil {
		return resp
	}
	// Say so BEFORE running: without this line a task the user expects
	// to queue detached silently blocks the terminal for the whole
	// prepare instead.
	PrintWarn("daemon unreachable — running in-process (blocks until done; `treeman daemon start` restores detached dispatch)")
	resp, err := daemon.RunPlanInProcess(ctx, *req.RunPlan)
	if err != nil {
		return rpc.Response{Kind: rpc.KindError, Message: err.Error()}
	}
	return resp
}

// subscribeRun opens a run-id-scoped event subscription, starting the
// daemon first if it isn't reachable.
func subscribeRun(ctx context.Context, runID string) (<-chan rpc.EventEnvelope, func(), error) {
	ch, cancel, err := rpc.SubscribeEvents(ctx, rpc.EventSubscribeArgs{RunID: runID})
	if err != nil {
		if startErr := wt2.EnsureDaemon(ctx); startErr != nil {
			return nil, nil, err
		}
		ch, cancel, err = rpc.SubscribeEvents(ctx, rpc.EventSubscribeArgs{RunID: runID})
	}
	return ch, cancel, err
}

// streamPlanEvents prints the live event tail for a foreground plan and
// returns when the daemon emits the terminal event: nil on plan:end,
// an error on a run_plan error event. A closed channel (daemon gone)
// returns a best-effort error.
func streamPlanEvents(ch <-chan rpc.EventEnvelope, label string) error {
	for ev := range ch {
		switch ev.EventType {
		case store.EvtPlanEnd:
			return nil
		case store.EvtPlanError:
			if ev.Level == store.LevelError {
				return fmt.Errorf("%s failed: %s", label, ev.Message)
			}
		}
		printTaskEvent(ev)
	}
	return fmt.Errorf("%s: daemon closed the event stream before completion", label)
}

// printTaskEvent renders one streamed event line for a foreground plan.
func printTaskEvent(ev rpc.EventEnvelope) {
	switch ev.Level {
	case store.LevelError, store.LevelWarn:
		PrintWarn("%s: %s", ev.EventType, ev.Message)
	default:
		PrintInfo("%s: %s", ev.EventType, ev.Message)
	}
}

// resolveWtRepo resolves a worktree arg (empty → cwd) to an absolute
// path + its repo root, for dispatch args the daemon can act on (the
// daemon's cwd is not the user's). Mirrors the resolution the prepare /
// db-reset cores do inline.
func resolveWtRepo(worktree, repoOverride string) (wtPath, repoRoot string, err error) {
	wtPath = worktree
	if wtPath == "" {
		cwd, _ := os.Getwd()
		wtPath = cwd
	}
	wtPath = MustAbs(wtPath)
	repoRoot, err = resolveRepo(repoOverride)
	if err != nil {
		repoRoot, err = DiscoverRepoRoot(wtPath)
	}
	return wtPath, repoRoot, err
}

// DbCmd — `treeman db reset` re-syncs a worktree's branch_scoped
// databases from the live base branch. Drops the current branch's
// durable copy + the active namespace, then re-runs prepare so each
// is repopulated from the live parent branch.
func DbCmd() *cli.Command {
	return &cli.Command{
		Name:  "db",
		Usage: "per-worktree database operations",
		Commands: []*cli.Command{
			{
				Name:      "reset",
				Usage:     "re-sync branch_scoped databases from the live base branch (defaults to the cwd's worktree)",
				ArgsUsage: "[worktree]",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
					&cli.StringFlag{
						Name:  "engine",
						Usage: "restrict the reset to one engine family (mysql, postgres, mongodb, redis, elasticsearch; aliases like mariadb/postgresql/valkey/dragonfly accepted)",
					},
					&cli.BoolFlag{Name: "json"},
					&cli.BoolFlag{
						Name:    "foreground",
						Aliases: []string{"wait", "f"},
						Usage:   "stream the daemon's live progress and block until done (default: dispatch and return)",
					},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					wtPath, repoRoot, err := resolveWtRepo(c.Args().First(), c.String("repo"))
					if err != nil {
						return err
					}
					task := rpc.Task{
						Type: rpc.TaskDBReset, RepoPath: repoRoot, WorktreePath: wtPath,
						Params:       map[string]string{rpc.ParamEngineFilter: strings.ToLower(c.String("engine"))},
						InheritedEnv: CaptureInheritedEnv(),
					}
					if c.Bool("json") {
						return runResultJSON(ctx, task)
					}
					return dispatchTask(ctx, task, c.Bool("foreground"), "db reset")
				},
			},
			{
				Name:      "status",
				Usage:     "show branch_scoped state: active namespace, current branch, and resumable branches (defaults to the cwd's worktree)",
				ArgsUsage: "[worktree]",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
					&cli.BoolFlag{Name: "json"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					dbs, err := RunDbStatusOnWorktree(ctx, c.Args().First(), c.String("repo"))
					if err != nil {
						return err
					}
					if c.Bool("json") {
						return jsonStream(map[string]any{"databases": dbs})
					}
					if len(dbs) == 0 {
						PrintInfo("no branch_scoped databases configured")
						return nil
					}
					for _, d := range dbs {
						active := d.ActiveBranch
						if active == "" {
							active = ui.Dim("(none)")
						} else {
							active = ui.Cyan(active)
						}
						ui.Plain("%s %s — active branch: %s; resumable: %s",
							ui.Cyan("["+d.Engine+"]"), ui.Bold(d.Active), active,
							ui.Dim(strings.Join(d.ResumableBranches, ", ")))
					}
					return nil
				},
			},
		},
	}
}

// RunDbStatusOnWorktree resolves the worktree + repo and reports the
// branch_scoped swap state per database. Shared by `treeman db status`
// and the MCP branch_scoped_status tool.
func RunDbStatusOnWorktree(ctx context.Context, worktree, repoOverride string) ([]prepare.BranchScopedDB, error) {
	wt := worktree
	if wt == "" {
		cwd, _ := os.Getwd()
		wt = cwd
	}
	wt = MustAbs(wt)
	repoRoot, err := resolveRepo(repoOverride)
	if err != nil {
		repoRoot, err = DiscoverRepoRoot(wt)
		if err != nil {
			return nil, err
		}
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, err
	}
	branch := detectBranchOfWorktree(wt)
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	id, err := wt2.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
	if err != nil {
		return nil, err
	}
	return prepare.BranchScopedStatus(ctx, &cfg, repoRoot, wt, id.WtID, st)
}

// HookCmd — `treeman hook run <phase>` runs the configured hooks
// for that phase against the cwd's repo + worktree.
func HookCmd() *cli.Command {
	return &cli.Command{
		Name:  "hook",
		Usage: "run a hook phase",
		Commands: []*cli.Command{{
			Name:  "run",
			Usage: "run a hook phase using the cwd's repo config",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "worktree", Aliases: []string{"w"}},
				&cli.BoolFlag{Name: "json"},
				&cli.BoolFlag{
					Name:    "foreground",
					Aliases: []string{"wait", "f"},
					Usage:   "stream the daemon's live progress and block until done (default: dispatch and return)",
				},
			},
			Action: func(ctx context.Context, c *cli.Command) error {
				if c.NArg() < 1 {
					return errors.New("usage: treeman hook run <phase>")
				}
				phase := c.Args().First()
				wtPath, repoRoot, err := resolveWtRepo(c.String("worktree"), "")
				if err != nil {
					return err
				}
				task := rpc.Task{
					Type: rpc.TaskHookRun, RepoPath: repoRoot, WorktreePath: wtPath,
					Params:       map[string]string{rpc.ParamPhase: phase},
					InheritedEnv: CaptureInheritedEnv(),
				}
				if c.Bool("json") {
					return runResultJSON(ctx, task)
				}
				return dispatchTask(ctx, task, c.Bool("foreground"), "hook "+phase)
			},
		}},
	}
}

// padRight pads a possibly-ANSI-colored cell with trailing spaces.
// Shared by the logs and hook tables, which both compose colored
// fixed-width columns by hand for compactness.
func padRight(cell string, width int) string {
	pad := width - ui.Width(cell)
	if pad <= 0 {
		return cell
	}
	return cell + strings.Repeat(" ", pad)
}

func formatTs(ms int64) string {
	// 2026-05-20 19:21:53.209
	sec := ms / 1000
	msPart := ms % 1000
	t := timeFromUnix(sec)
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%03d",
		t.Y, t.M, t.D, t.H, t.Min, t.S, msPart)
}

type tparts struct{ Y, M, D, H, Min, S int }

func timeFromUnix(sec int64) tparts {
	// Stdlib time package handles tz; use UTC for stable output.
	t := timeFromUnixUTC(sec)
	return t
}

// ConfigCmd — `treeman config {validate,show}` echoes the merged
// config (and optionally the resolved connections).
func ConfigCmd() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "config helpers",
		Commands: []*cli.Command{
			{
				Name:  "validate",
				Usage: "validate the config loads without error",
				Flags: []cli.Flag{&cli.BoolFlag{Name: "json"}},
				Action: func(ctx context.Context, c *cli.Command) error {
					repoRoot, err := resolveRepo("")
					if err != nil {
						if c.Bool("json") {
							return jsonStream(map[string]any{"ok": false, "error": err.Error()})
						}
						return err
					}
					cfg, err := resolve.LoadResolved(repoRoot)
					if err != nil {
						if c.Bool("json") {
							return jsonStream(map[string]any{"ok": false, "repo": repoRoot, "error": err.Error()})
						}
						return err
					}
					if c.Bool("json") {
						return jsonStream(map[string]any{
							"ok":        true,
							"repo":      repoRoot,
							"databases": len(cfg.Databases),
						})
					}
					PrintOK("config loaded (%d databases configured)", len(cfg.Databases))
					return nil
				},
			},
			{
				Name:  "show",
				Usage: "dump the resolved config",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "resolved"},
					&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
					&cli.BoolFlag{Name: "json"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					repoRoot, err := resolveRepo("")
					if err != nil {
						return err
					}
					cfg, err := resolve.LoadResolved(repoRoot)
					if err != nil {
						return err
					}
					if c.Bool("json") {
						out := map[string]any{"repo": repoRoot, "config": cfg}
						if c.Bool("resolved") {
							out["resolved"] = resolve.Resolve(&cfg, repoRoot)
						}
						return jsonStream(out)
					}
					pager := newPagerIfEligible(c, false, false)
					if pager != nil {
						_ = pager.Start()
						defer pager.Close()
					}
					b, _ := yaml.Marshal(cfg)
					_, _ = fmt.Fprint(ui.Out, string(b))
					if c.Bool("resolved") {
						r := resolve.Resolve(&cfg, repoRoot)
						_, _ = fmt.Fprintln(ui.Out)
						_, _ = fmt.Fprintln(ui.Out, "# resolved connections")
						printResolved("mysql", r.Mysql)
						printResolved("postgres", r.Postgres)
						printResolved("mongodb", r.Mongodb)
						printResolved("redis", r.Redis)
						printResolved("elasticsearch", r.Elasticsearch)
					}
					return nil
				},
			},
			configSet(),
			configHistory(),
			configRestore(),
		},
	}
}

// configHistory returns `treeman config history` — lists the per-repo
// generations of `.treeman.yaml` stashed in SQLite each time the file
// was overwritten by `config set` / `config write` / `main enable`.
func configHistory() *cli.Command {
	return &cli.Command{
		Name:  "history",
		Usage: "list stored .treeman.yaml generations for this repo",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			p := filepath.Join(repoRoot, ".treeman.yaml")
			st, err := openDefaultStore(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			gens, err := st.ListConfigGenerations(ctx, repoRoot, p)
			if err != nil {
				return err
			}
			if c.Bool("json") {
				rows := make([]map[string]any, 0, len(gens))
				for _, g := range gens {
					rows = append(rows, map[string]any{
						"generation": g.Generation,
						"created_at": time.UnixMilli(g.CreatedAt).UTC().Format(time.RFC3339),
						"bytes":      len(g.Content),
					})
				}
				return jsonStream(map[string]any{"repo": repoRoot, "generations": rows})
			}
			if len(gens) == 0 {
				ui.Info("no stored generations for %s", p)
				return nil
			}
			for _, g := range gens {
				_, _ = fmt.Fprintf(ui.Out, "  %s  %s  %d bytes\n",
					ui.Bold(fmt.Sprintf("gen %d", g.Generation)),
					time.UnixMilli(g.CreatedAt).Local().Format("2006-01-02 15:04:05"),
					len(g.Content))
			}
			ui.Hint("restore one with `treeman config restore <generation>`")
			return nil
		},
	}
}

// configRestore returns `treeman config restore <generation>` — writes
// a stored generation back to `.treeman.yaml`. The current content is
// itself snapshotted first, so a restore is reversible.
func configRestore() *cli.Command {
	return &cli.Command{
		Name:      "restore",
		Usage:     "write a stored generation back to .treeman.yaml",
		ArgsUsage: "<generation>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman config restore <generation>")
			}
			gen, err := strconv.ParseInt(c.Args().Get(0), 10, 64)
			if err != nil {
				return fmt.Errorf("generation must be an integer: %w", err)
			}
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			p := filepath.Join(repoRoot, ".treeman.yaml")
			st, err := openDefaultStore(ctx)
			if err != nil {
				return err
			}
			g, err := st.GetConfigGeneration(ctx, repoRoot, p, gen)
			_ = st.Close()
			if err != nil {
				return fmt.Errorf("generation %d not found for %s", gen, p)
			}
			if err := config.CheckBodyScope(g.Content, "repo"); err != nil {
				return fmt.Errorf("generation %d cannot be restored: %w", gen, err)
			}
			if err := writeConfig(ctx, repoRoot, p, g.Content); err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(map[string]any{
					"repo":     repoRoot,
					"restored": gen,
					"bytes":    len(g.Content),
				})
			}
			PrintOK("restored generation %d → %s (%d bytes)", gen, p, len(g.Content))
			return nil
		},
	}
}

func printResolved(name string, c any) {
	// `c` is a pointer to a resolvedConn[…] struct from
	// internal/resolve. We only need a string representation; JSON
	// is good enough. Write through ui.Out so the pager wrap in
	// `config show` captures these lines too.
	if isNil(c) {
		_, _ = fmt.Fprintf(ui.Out, "# %s <- (none)\n", name)
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		_, _ = fmt.Fprintf(ui.Out, "# %s <- (unprintable: %v)\n", name, err)
		return
	}
	_, _ = fmt.Fprintf(ui.Out, "# %s <- %s\n", name, string(b))
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	if x, ok := v.(interface{ IsNil() bool }); ok {
		return x.IsNil()
	}
	// Reflection-free heuristic: stringify and look for the typed-
	// nil shape `(*…)(nil)` — good enough for this CLI helper.
	s := fmt.Sprintf("%v", v)
	return s == "<nil>" || s == ""
}

// SchemaCmd — `treeman schema {dump,install}`. Generates the
// JSON Schema for `.treeman.yaml` directly from the config.Config
// type via reflection.
//
//	dump             → write schema to stdout
//	dump --out P     → write schema to file P
//	install          → write schema to `<repo>/schemas/treeman.schema.json`
//	install --global → write schema to the user-global path (XDG)
//	install --url    → don't write a file; point the modeline at the
//	                   upstream URL on GitHub
//
// `install` always updates (or inserts) the
// `# yaml-language-server: $schema=...` modeline at the top of
// `.treeman.yaml` to match the chosen target so editor hinting
// picks the right source without manual edits.
func SchemaCmd() *cli.Command {
	return &cli.Command{
		Name:  "schema",
		Usage: "JSON schema helpers",
		Commands: []*cli.Command{
			{
				Name: "dump",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "out"},
					&cli.StringFlag{
						Name:  "scope",
						Value: "full",
						Usage: "full (default, every key) | global (~/.config/treeman/config.yaml keys) | repo (.treeman.yaml keys)",
					},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					var sc schema.Scope
					switch c.String("scope") {
					case "", "full":
						sc = schema.ScopeFull
					case "global":
						sc = schema.ScopeGlobal
					case "repo":
						sc = schema.ScopeRepo
					default:
						return fmt.Errorf("invalid --scope %q (want full|global|repo)", c.String("scope"))
					}
					b, err := schema.RenderScoped(sc)
					if err != nil {
						return err
					}
					if out := c.String("out"); out != "" {
						return os.WriteFile(out, b, 0o644)
					}
					_, err = os.Stdout.Write(append(b, '\n'))
					return err
				},
			},
			{
				Name: "install",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "global", Usage: "write to the user-global path instead of <repo>/schemas/"},
					&cli.BoolFlag{Name: "url", Usage: "skip the file write; point the modeline at the upstream URL"},
				},
				Action: schemaInstall,
			},
		},
	}
}

func schemaInstall(ctx context.Context, c *cli.Command) error {
	if c.Bool("global") && c.Bool("url") {
		return errors.New("--global and --url are mutually exclusive")
	}
	cwd, _ := os.Getwd()
	repoRoot, err := DiscoverRepoRoot(cwd)
	if err != nil {
		return err
	}
	var target schema.Target
	switch {
	case c.Bool("url"):
		target = schema.TargetURL
	case c.Bool("global"):
		target = schema.TargetGlobal
	default:
		target = schema.TargetRepo
	}
	resolved, changed, err := schema.Install(repoRoot, target)
	if err != nil {
		return err
	}
	if target == schema.TargetURL {
		PrintOK("using upstream URL: %s", resolved)
	} else {
		PrintOK("wrote %s", resolved)
	}
	if changed {
		// The modeline file depends on the target: --global rewrites the
		// user-global config, every other target rewrites the repo
		// `.treeman.yaml`. Report the file actually edited.
		modelineFile := ".treeman.yaml"
		if target == schema.TargetGlobal {
			if gp, ok := config.GlobalConfigPath(); ok {
				modelineFile = gp
			}
		}
		PrintOK("updated modeline in %s → %s", modelineFile, resolved)
	}
	return nil
}

// DaemonCmd — `treeman daemon {start,stop,reload,status,install,uninstall}`.
func DaemonCmd() *cli.Command {
	return &cli.Command{
		Name:  "daemon",
		Usage: "daemon lifecycle",
		Commands: []*cli.Command{
			{Name: "start", Action: daemonStart},
			{Name: "stop", Action: daemonStop},
			{
				Name:  "reload",
				Usage: "ask the daemon to re-read config + restart watchers (no process restart)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "limit reload to one repo path; defaults to all"},
				},
				Action: daemonReload,
			},
			{
				Name:   "status",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json"}},
				Action: daemonStatus,
			},
			{
				Name:   "state",
				Usage:  "live runtime snapshot — watchers, in-flight finalizes/teardowns, auto-fetch backoffs (CLI twin of the MCP daemon_state tool)",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json"}},
				Action: daemonState,
			},
			{Name: "install", Action: daemonInstall},
			{
				Name: "uninstall",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
				},
				Action: daemonUninstall,
			},
		},
	}
}

func daemonReload(ctx context.Context, c *cli.Command) error {
	repoPath := c.String("repo")
	if repoPath != "" {
		var err error
		repoPath, err = resolveRepo(repoPath)
		if err != nil {
			return err
		}
	}
	resp, err := rpc.Call(ctx, rpc.Request{
		Method:       rpc.MethodConfigReload,
		ConfigReload: &rpc.ConfigReloadArgs{RepoPath: repoPath},
	})
	if err != nil {
		return err
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	if repoPath == "" {
		PrintOK("config reload requested (all repos)")
	} else {
		PrintOK("config reload requested (%s)", repoPath)
	}
	return nil
}

// daemonState — `treeman daemon state` prints the daemon's live
// runtime snapshot. Answers "is a finalize already running for this
// worktree / why isn't this repo auto-fetching?" without reaching for
// the MCP server.
func daemonState(ctx context.Context, c *cli.Command) error {
	resp, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodDaemonState})
	if err != nil {
		return fmt.Errorf("daemon not reachable: %w (start it with `treeman daemon start`)", err)
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	if c.Bool("json") {
		return jsonStream(resp.State)
	}
	st := resp.State
	if st == nil {
		PrintInfo("daemon returned no state snapshot")
		return nil
	}
	_, _ = fmt.Fprintf(ui.Out, "%s %d repo watcher(s)\n", ui.Bold("watchers:"), st.WatcherCount)
	for _, w := range st.Watchers {
		_, _ = fmt.Fprintf(ui.Out, "  %s (%d worktree(s))\n", w.Repo, w.WorktreeCount)
	}
	if len(st.WorktreeWatchers) > 0 {
		_, _ = fmt.Fprintf(ui.Out, "%s %s\n", ui.Bold("worktree watchers:"), strings.Join(st.WorktreeWatchers, ", "))
	}
	if len(st.LifecycleWatchers) > 0 {
		_, _ = fmt.Fprintf(ui.Out, "%s %s\n", ui.Bold("lifecycle watchers:"), strings.Join(st.LifecycleWatchers, ", "))
	}
	if len(st.InFlightFinalizes) > 0 {
		_, _ = fmt.Fprintln(ui.Out, ui.Bold("in-flight finalizes:"))
		for _, f := range st.InFlightFinalizes {
			_, _ = fmt.Fprintf(ui.Out, "  %s (running %ds)\n", f.WorktreePath, f.AgeSeconds)
		}
	}
	if len(st.InFlightTeardowns) > 0 {
		_, _ = fmt.Fprintf(ui.Out, "%s %s\n", ui.Bold("in-flight teardowns:"), strings.Join(st.InFlightTeardowns, ", "))
	}
	if len(st.SyncBackoffs) > 0 {
		_, _ = fmt.Fprintln(ui.Out, ui.Bold("auto-fetch backoffs:"))
		for _, b := range st.SyncBackoffs {
			next := "eligible now"
			if b.NextRetryUnix > 0 {
				next = "next retry " + time.Unix(b.NextRetryUnix, 0).Format(time.TimeOnly)
			}
			_, _ = fmt.Fprintf(ui.Out, "  %s: %d consecutive failure(s), %s\n", b.RepoPath, b.ConsecFailures, next)
		}
	}
	for repo, reason := range st.SyncLastSkips {
		_, _ = fmt.Fprintf(ui.Out, "%s %s: %s\n", ui.Bold("last fetch skip:"), repo, reason)
	}
	if st.WatcherCount == 0 && len(st.InFlightFinalizes) == 0 && len(st.InFlightTeardowns) == 0 {
		PrintInfo("daemon idle — no watchers or in-flight work")
	}
	return nil
}

func daemonStart(ctx context.Context, c *cli.Command) error {
	pid, err := startDaemonProcess(ctx)
	if err != nil {
		return err
	}
	if pid > 0 {
		PrintOK("treemand started (pid %d)", pid)
	}
	return nil
}

// startDaemonProcess is a thin wrapper around daemonctl.Start kept
// so the existing auto-recover paths in wt create / wt delete can
// keep calling it without an import churn.
func startDaemonProcess(ctx context.Context) (int, error) {
	return daemonctl.Start(ctx)
}

func daemonStop(ctx context.Context, c *cli.Command) error {
	if err := daemonctl.Stop(ctx); err != nil {
		return err
	}
	PrintOK("daemon shutdown requested")
	return nil
}

func daemonStatus(ctx context.Context, c *cli.Command) error {
	resp, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodStatus})
	if c.Bool("json") {
		out := map[string]any{}
		if err != nil {
			out["status"] = "not-running"
			out["error"] = err.Error()
		} else {
			out["status"] = "running"
			out["version"] = resp.DaemonVersion
			out["pid"] = resp.Pid
			out["watchers"] = resp.WatcherCount
		}
		return jsonStream(out)
	}
	if err != nil {
		ui.Warn("daemon not running")
		ui.Hint("start it with: treeman daemon start")
		ui.Hint("or auto-launch on login: treeman daemon install")
		return nil //nolint:nilerr // "not running" is a normal status result, not a CLI error
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	ui.Success("treemand %s — pid=%d watchers=%d", ui.Bold(resp.DaemonVersion), resp.Pid, resp.WatcherCount)
	return nil
}

// daemonInstall writes the user-mode unit/plist appropriate for
// the host OS and enables it. Linux gets a systemd-user unit; macOS
// gets a LaunchAgent plist + `launchctl bootload`.
func daemonInstall(ctx context.Context, c *cli.Command) error {
	switch runtime.GOOS {
	case "darwin":
		return daemonInstallLaunchd(ctx)
	default:
		return daemonInstallSystemd(ctx)
	}
}

func daemonUninstall(ctx context.Context, c *cli.Command) error {
	if !c.Bool("yes") {
		if !ui.Confirm("remove the treemand systemd/launchd auto-start unit?") {
			return errors.New("aborted")
		}
	}
	switch runtime.GOOS {
	case "darwin":
		return daemonUninstallLaunchd(ctx)
	default:
		return daemonUninstallSystemd(ctx)
	}
}

// The systemd/launchd install/uninstall implementations live in the
// daemonctl package so the MCP daemon_control tool shares them. These
// thin wrappers print the daemonctl summary; the test suite calls them
// directly to exercise both OS branches on a single host.

func daemonInstallSystemd(ctx context.Context) error {
	msg, err := daemonctl.InstallSystemd(ctx)
	if err == nil {
		PrintOK("%s", msg)
	}
	return err
}

func daemonUninstallSystemd(ctx context.Context) error {
	msg, err := daemonctl.UninstallSystemd(ctx)
	if err == nil {
		PrintOK("%s", msg)
	}
	return err
}

func daemonInstallLaunchd(ctx context.Context) error {
	msg, err := daemonctl.InstallLaunchd(ctx)
	if err == nil {
		PrintOK("%s", msg)
	}
	return err
}

func daemonUninstallLaunchd(ctx context.Context) error {
	msg, err := daemonctl.UninstallLaunchd(ctx)
	if err == nil {
		PrintOK("%s", msg)
	}
	return err
}

// daemonAutoStartInstalled reports whether a systemd-user or launchd
// auto-start unit for treemand is already on disk. Used by `treeman
// init` to suppress the "install the daemon" hint when the user has
// already set it up (or has it managed declaratively, e.g. by
// home-manager). Existence-only — does not check whether the unit is
// currently active.
func daemonAutoStartInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	candidates := []string{
		filepath.Join(home, ".config", "systemd", "user", "treemand.service"),
		filepath.Join(home, "Library", "LaunchAgents", daemonctl.LaunchdLabel+".plist"),
	}
	for _, p := range candidates {
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}
	return false
}

// FwCmd — `treeman fw detect` lists detected migration + test
// frameworks.
func FwCmd() *cli.Command {
	return &cli.Command{
		Name:    "frameworks",
		Aliases: []string{"fw"},
		Usage:   "framework detection",
		Commands: []*cli.Command{{
			Name:  "detect",
			Flags: []cli.Flag{&cli.BoolFlag{Name: "json"}},
			Action: func(ctx context.Context, c *cli.Command) error {
				cwd, _ := os.Getwd()
				repoRoot, err := DiscoverRepoRoot(cwd)
				if err != nil {
					return err
				}
				// Honour the user's `frameworks:` block alongside the
				// built-in detectors.
				cfg, _ := resolve.LoadResolved(repoRoot)
				r := framework.RegistryFor(&cfg)
				detected := r.DetectAll(repoRoot)
				if c.Bool("json") {
					return jsonStream(map[string]any{
						"repo":              repoRoot,
						"migration":         detected,
						"test":              testfw.DetectAll(repoRoot),
						"auto_clone_target": testfw.DetectedCloneCount(repoRoot),
					})
				}
				migs := ui.NewTable("MIGRATION_FW", "ON_MODIFY", "DIRS")
				for _, s := range detected {
					migs.Row(ui.Cyan(s.Name), string(s.OnModify), ui.Dim(strings.Join(s.MigrationDirs, ",")))
				}
				if len(detected) == 0 {
					ui.Info("no migration framework detected in %s", repoRoot)
				} else {
					migs.Render(nil)
				}
				ui.Plain("")
				tests := ui.NewTable("TEST_FW", "LANGUAGE", "STRATEGY", "WORKER_IDX", "WORKER_ENV")
				detTests := testfw.DetectAll(repoRoot)
				for _, t := range detTests {
					tests.Row(ui.Cyan(t.Name), t.Language, string(t.Strategy), string(t.WorkerIndex), ui.Dim(t.WorkerEnv))
				}
				if len(detTests) == 0 {
					ui.Info("no test framework detected")
				} else {
					tests.Render(nil)
				}
				ui.Plain("")
				ui.Plain("auto-clones (replication target): %s",
					ui.Bold(strconv.FormatUint(uint64(testfw.DetectedCloneCount(repoRoot)), 10)))
				return nil
			},
		}},
	}
}

// SlugCmd — `treeman slug [path]` prints the slug of a worktree.
func SlugCmd() *cli.Command {
	return &cli.Command{
		Name:  "slug",
		Usage: "print the slug treeman derives for a worktree",
		Flags: []cli.Flag{&cli.BoolFlag{Name: "json"}},
		Action: func(ctx context.Context, c *cli.Command) error {
			path := "."
			if c.NArg() >= 1 {
				path = c.Args().First()
			}
			path = MustAbs(path)
			branch := detectBranchOfWorktree(path)
			sl := slug.For(path, branch)
			if c.Bool("json") {
				q, ca := sl.RedisIndices()
				return jsonStream(map[string]any{
					"slug":              sl.Value,
					"path":              path,
					"branch":            branch,
					"redis_queue_index": q,
					"redis_cache_index": ca,
				})
			}
			fmt.Println(sl.Value)
			return nil
		},
	}
}

// InitCmd — `treeman init` scaffolds a `.treeman.yaml` tailored to
// what the cwd looks like:
//
//   - Markers (artisan, package.json, composer.json, go.mod, …)
//     decide which framework block to emit.
//   - Lockfiles (composer.lock, yarn.lock, pnpm-lock.yaml,
//     package-lock.json, bun.lockb, deno.lock, go.sum) decide which
//     package-manager + JS-runtime install lines go in the
//     postcreate hook groups.
//   - The `hooks.postcreate` block uses groups so installs run in
//     parallel (e.g. composer + yarn root + yarn frontend three
//     groups, each sequenced inside) — matches the "sequence-within-
//     group, parallel-across-groups" contract.
//
// Errors only when `.treeman.yaml` already exists without `--force`.
// Detection is best-effort: emit what we can, comment what we can't.
func InitCmd() *cli.Command {
	return &cli.Command{
		Name: "init",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "force"},
			&cli.BoolFlag{Name: "json"},
			&cli.BoolFlag{
				Name:  "global",
				Usage: "scaffold the user-global ~/.config/treeman/config.yaml (machine-wide defaults) instead of a per-repo .treeman.yaml",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Bool("global") {
				return initGlobalAction(c)
			}
			cwd, _ := os.Getwd()
			detected := framework.DefaultRegistry().DetectAll(cwd)
			target, created, body, err := InitTreemanYAML(cwd, c.Bool("force"))
			if err != nil {
				return err
			}
			if c.Bool("json") {
				names := make([]string, 0, len(detected))
				for _, s := range detected {
					names = append(names, s.Name)
				}
				return jsonStream(map[string]any{
					"path":     target,
					"created":  created,
					"bytes":    len(body),
					"detected": names,
				})
			}
			if len(detected) > 0 {
				names := make([]string, 0, len(detected))
				for _, s := range detected {
					names = append(names, s.Name)
				}
				PrintInfo("detected: %s", strings.Join(names, ", "))
			} else {
				PrintWarn("no migration framework detected — the databases: block was left commented out")
				PrintHint("see `treeman fw list` for built-in presets, or author databases: by hand")
			}
			PrintOK("wrote %s", target)
			PrintHint("review the generated databases:/hooks: blocks before first create")
			if !daemonAutoStartInstalled() {
				PrintHint("install the daemon (one-time): treeman daemon install")
			}
			PrintHint("create a worktree:               treeman wt create <branch>")
			PrintHint("install JSON Schema (editors):   treeman schema install")
			return nil
		},
	}
}

// InitTreemanYAML is a thin shim over initgen.WriteYAML kept so
// older callers that import this symbol keep compiling. New code
// should depend on internal/initgen directly.
func InitTreemanYAML(cwd string, force bool) (path string, created bool, body string, err error) {
	return initgen.WriteYAML(cwd, force)
}

// initGlobalAction scaffolds the user-global config.yaml and installs
// the matching global-scoped JSON Schema beside it so editors validate
// against the narrower key set (repo-only keys flagged as unknown).
func initGlobalAction(c *cli.Command) error {
	target, created, body, err := initgen.WriteGlobalYAML(c.Bool("force"))
	if err != nil {
		return err
	}
	// Install the global-scoped schema next to the config and point its
	// modeline at it. Install resolves the modeline file from the target,
	// so for TargetGlobal it edits the global config we just wrote.
	schemaPath, _, schemaErr := schema.Install("", schema.TargetGlobal)
	if c.Bool("json") {
		out := map[string]any{
			"path":    target,
			"created": created,
			"bytes":   len(body),
			"scope":   "global",
		}
		if schemaErr == nil {
			out["schema"] = schemaPath
		}
		return jsonStream(out)
	}
	PrintOK("wrote %s", target)
	if schemaErr == nil {
		PrintHint("installed global schema:          %s", schemaPath)
	}
	PrintHint("machine-wide defaults only — repo-specific blocks go in each .treeman.yaml")
	return nil
}

// detectBranchOfWorktree reads .git/HEAD (handles gitlink files for
// linked worktrees) and returns the branch or "".
func detectBranchOfWorktree(worktree string) string {
	head := filepath.Join(worktree, ".git", "HEAD")
	if _, err := os.Stat(head); err != nil {
		// gitlink
		link, err := os.ReadFile(filepath.Join(worktree, ".git"))
		if err != nil {
			return ""
		}
		gitdir := strings.TrimSpace(strings.TrimPrefix(string(link), "gitdir:"))
		head = filepath.Join(gitdir, "HEAD")
	}
	b, err := os.ReadFile(head)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	const pfx = "ref: refs/heads/"
	if after, ok := strings.CutPrefix(s, pfx); ok {
		return after
	}
	return ""
}
