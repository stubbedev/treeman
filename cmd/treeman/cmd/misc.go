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
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/initgen"
	mcpsrv "github.com/stubbedev/treeman/internal/mcp"
	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/schema"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/tui"
	"github.com/stubbedev/treeman/internal/ui"
	wt2 "github.com/stubbedev/treeman/internal/wt"
	"github.com/stubbedev/treeman/internal/yamlpatch"
	"github.com/stubbedev/treeman/pkg/rpc"
)

// PrepareCmd — `treeman prepare` runs the full pipeline foreground.
func PrepareCmd() *cli.Command {
	return &cli.Command{
		Name:  "prepare",
		Usage: "ensure → dump → migrate → snapshot → replicate",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "worktree", Aliases: []string{"w"}},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "json"},
			&cli.BoolFlag{
				Name:  "no-daemon",
				Usage: "run synchronously in this process without connecting to or starting a daemon (for CI)",
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "render the per-database pipeline plan (source dbs, dumps, migrate/seed commands, fanout) without executing anything",
			},
			&cli.BoolFlag{
				Name:  "fresh",
				Usage: "restore every templated database and its test clones from the cached template even when they are already at the current fingerprint (discards state left by earlier test runs)",
			},
			foregroundFlag(),
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			wtPath, repoRoot, err := resolveWtRepo(c.String("worktree"), c.String("repo"))
			if err != nil {
				return err
			}
			// --dry-run (#60): the MCP prepare_dry_run core, in-process.
			if c.Bool("dry-run") {
				return previewPrepare(ctx, c, wtPath, repoRoot)
			}
			task := rpc.PrepareArgs{Fresh: c.Bool("fresh")}.Task(repoRoot, wtPath, CaptureInheritedEnv())
			if c.Bool("no-daemon") {
				resp, err := daemon.RunPlanInProcess(ctx, *rpc.Plan(true, rpc.One(task)).RunPlan)
				if err != nil {
					return err
				}
				if !c.Bool("json") {
					return reportInlineResult(resp, "prepare")
				}
				for _, result := range resp.TaskResults {
					if !result.OK {
						return fmt.Errorf("%s: %s", result.Type, result.Message)
					}
					if _, err := fmt.Fprintln(ui.Out, result.PayloadJSON); err != nil {
						return err
					}
				}
				return nil
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
// The subscription is acknowledged before dispatch (rpc.SubscribeEvents
// returns only once the daemon's filter is live), so even a plan that
// fails instantly delivers its terminal event. Only an unreachable
// daemon falls back in-process; any other subscribe or dispatch failure
// is surfaced, since the daemon may be running the plan and a second,
// in-process run would race its writer.
func foregroundTask(ctx context.Context, task rpc.Task, label string) error {
	runID := runid.New()
	ch, cancel, err := subscribeRun(ctx, runID)
	if err != nil {
		if !errors.Is(err, rpc.ErrDaemonUnreachable) {
			return fmt.Errorf("%s: %w", label, err)
		}
		return reportInlineResult(submitPlan(ctx, rpc.Plan(true, rpc.One(task))), label)
	}
	defer cancel()
	req := rpc.Plan(false, rpc.One(task))
	req.RunPlan.RunID = runID
	resp := submitPlan(ctx, req)
	switch resp.Kind {
	case rpc.KindPlanQueued:
		return streamPlanEvents(ch, label)
	case rpc.KindPlanResult:
		// The daemon went away between subscribe and dispatch;
		// submitPlan ran the plan in-process to completion.
		return reportInlineResult(resp, label)
	default:
		return fmt.Errorf("%s: %s", label, resp.Message)
	}
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

// strictDaemon records whether this invocation demands a live daemon
// (--require-daemon flag or TREEMAN_REQUIRE_DAEMON=1). In strict mode
// submitPlan refuses the in-process fallback, so scripts fail fast and
// loud when treemand is down instead of silently changing latency and
// single-writer semantics. Package state, set once in the root
// command's Before — same lifecycle as the color mode.
var strictDaemon bool

// SetStrictDaemon pins strict-daemon mode for this invocation.
func SetStrictDaemon(v bool) { strictDaemon = v }

// StrictDaemon reports whether the in-process fallback is disabled.
func StrictDaemon() bool { return strictDaemon }

// submitPlan runs a plan: dispatch to the daemon when reachable, else
// execute it in-process. treeman works daemon-less — the daemon is the
// preferred mutator (fast async return, single writer), not a hard
// requirement. Always yields a response: KindPlanQueued from a live
// daemon, KindPlanResult from in-process, or KindError.
//
// Strict mode (--require-daemon / TREEMAN_REQUIRE_DAEMON=1, #75) opts
// out of the fallback: an unreachable daemon is a hard error naming
// `treeman daemon start`, so scripts get a fast, loud failure instead
// of a silent latency/semantics change (in-process blocks until done
// and bypasses the daemon's single-writer guarantees).
func submitPlan(ctx context.Context, req rpc.Request) rpc.Response {
	resp, err := rpc.Call(ctx, req)
	if err == nil {
		return resp
	}
	// Only fall back in-process when the DIAL failed (daemon genuinely
	// absent). A post-dial failure — encode/decode/timeout — means the
	// daemon is reachable and MAY already be running this plan; re-running
	// it in-process would put a second writer on the same rows and race
	// the daemon (e.g. UNIQUE constraint on worktrees.path). Surface it.
	if !errors.Is(err, rpc.ErrDaemonUnreachable) {
		return rpc.Response{Kind: rpc.KindError, Message: err.Error()}
	}
	if StrictDaemon() {
		return rpc.Response{
			Kind:    rpc.KindError,
			Message: err.Error() + " — strict mode (--require-daemon): refusing to run in-process; start it with `treeman daemon start`",
		}
	}
	// Say so BEFORE running: without this line a task the user expects
	// to queue detached silently blocks the terminal for the whole
	// prepare instead.
	PrintWarn("daemon unreachable — running in-process (blocks until done; `treeman daemon start` restores detached dispatch)")
	resp, err = daemon.RunPlanInProcess(ctx, *req.RunPlan)
	if err != nil {
		return rpc.Response{Kind: rpc.KindError, Message: err.Error()}
	}
	return resp
}

// subscribeRun opens a run-id-scoped event subscription, starting the
// daemon first if it isn't reachable. A daemon that is reachable but
// fails the subscription is reported as-is — restarting it here would
// hide the fault.
func subscribeRun(ctx context.Context, runID string) (<-chan rpc.EventEnvelope, func(), error) {
	args := rpc.EventSubscribeArgs{RunID: runID}
	ch, cancel, err := rpc.SubscribeEvents(ctx, args)
	if err == nil || !errors.Is(err, rpc.ErrDaemonUnreachable) {
		return ch, cancel, err
	}
	if startErr := wt2.EnsureDaemon(ctx); startErr != nil {
		return nil, nil, err
	}
	return rpc.SubscribeEvents(ctx, args)
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

// previewPrepare implements `prepare --dry-run` (#60): the MCP
// prepare_dry_run core in-process — per-database source dbs, dumps,
// migrate/seed commands and fanout, nothing executed.
func previewPrepare(_ context.Context, c *cli.Command, wtPath, repoRoot string) error {
	plan, err := mcpsrv.PrepareDryRunPlan(context.Background(), wtPath, repoRoot)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return json.NewEncoder(ui.Out).Encode(plan)
	}
	PrintInfo("prepare plan for %s (repo %s, slug %s) — nothing was executed:", plan.WorktreePath, plan.Repo, plan.Slug)
	for _, db := range plan.Databases {
		detail := db.SourceDB
		if db.KeyPrefix != "" {
			detail = db.KeyPrefix
		}
		if db.BranchScoped {
			detail += " (branch_scoped)"
		}
		if db.FanoutTarget > 0 {
			detail = fmt.Sprintf("%s + %d clone(s)", detail, db.FanoutTarget)
		}
		PrintInfo("  %s → %s", ui.Bold(db.Engine), detail)
		if db.Migrate != nil {
			ui.Hint("migrate: %s", db.Migrate.Run)
		}
		if db.Seed != nil {
			ui.Hint("seed: %s", db.Seed.Run)
		}
	}
	return nil
}

// DbCmd — `treeman db reset` re-syncs a worktree's branch_scoped
// databases from the live base branch. Drops the current branch's
// durable copy + the active namespace, then re-runs prepare so each
// is repopulated from the live parent branch. `treeman db save`
// captures the active namespaces into the current branch's durable
// copies without switching branches.
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
						Name:  "dry-run",
						Usage: "list the branch_scoped namespaces that would be dropped + re-seeded; change nothing, connect nowhere",
					},
					foregroundFlag(),
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					wtPath, repoRoot, err := resolveWtRepo(c.Args().First(), c.String("repo"))
					if err != nil {
						return err
					}
					// --dry-run (#60): same preview MCP db_reset's
					// dry_run returns — pure name math, no daemon dispatch,
					// no engine connections.
					if c.Bool("dry-run") {
						return previewBranchReset(ctx, c, repoRoot, wtPath)
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
				Name:      "save",
				Usage:     "capture branch_scoped databases into the current branch's durable copies without switching (defaults to the cwd's worktree)",
				ArgsUsage: "[worktree]",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
					&cli.StringFlag{
						Name:  "engine",
						Usage: "restrict the save to one engine family (mysql, postgres, mongodb, redis, elasticsearch; aliases like mariadb/postgresql/valkey/dragonfly accepted)",
					},
					&cli.BoolFlag{Name: "json"},
					foregroundFlag(),
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					wtPath, repoRoot, err := resolveWtRepo(c.Args().First(), c.String("repo"))
					if err != nil {
						return err
					}
					task := rpc.Task{
						Type: rpc.TaskDBSave, RepoPath: repoRoot, WorktreePath: wtPath,
						Params:       map[string]string{rpc.ParamEngineFilter: strings.ToLower(c.String("engine"))},
						InheritedEnv: CaptureInheritedEnv(),
					}
					if c.Bool("json") {
						return runResultJSON(ctx, task)
					}
					return dispatchTask(ctx, task, c.Bool("foreground"), "db save")
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
	st, err := openDefaultStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return nil, fmt.Errorf("register repo %s: %w", repoRoot, err)
	}
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
			Name:      "run",
			Usage:     "run a hook phase using the cwd's repo config",
			ArgsUsage: "<phase>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "worktree", Aliases: []string{"w"}},
				&cli.BoolFlag{Name: "json"},
				foregroundFlag(),
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

// formatTsStyled renders an event timestamp for human log lines:
// today's events print bare time-of-day (15:04:05) — the date is
// noise when tailing the current session — while older events keep
// the full stamp. fullTS always restores the wide format (#81).
func formatTsStyled(fullTS bool, ms int64) string {
	if fullTS {
		return formatTs(ms)
	}
	t := timeFromUnix(ms / 1000)
	now := timeFromUnix(time.Now().Unix())
	if t.Y == now.Y && t.M == now.M && t.D == now.D {
		return fmt.Sprintf("%02d:%02d:%02d", t.H, t.Min, t.S)
	}
	return formatTs(ms)
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
						printResolved("s3", r.S3)
					}
					return nil
				},
			},
			configSet(),
			configUnset(),
			configHistory(),
			configRestore(),
		},
	}
}

// configUnset returns `treeman config unset <path>` — the YAML twin
// of `config set` for removal. Deletes the key or sequence element
// via yamlpatch.Unset (comments + sibling order preserved), then
// persists through the same scope resolution + daemon snapshot +
// reload as set, so an unpatch lands in `config history` like any
// other write.
func configUnset() *cli.Command {
	return &cli.Command{
		Name:      "unset",
		Usage:     "remove a key or sequence element from the config by dotted path (comments preserved, prior content lands in history)",
		ArgsUsage: "<path>",
		Description: `Examples:
  treeman config unset worktrees.links
  treeman config unset patches[0]
  treeman config unset --global daemon.log_level`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "json"},
			&cli.BoolFlag{Name: "global", Usage: "edit the user-global ~/.config/treeman/config.yaml instead of .treeman.yaml"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman config unset <path>")
			}
			path := c.Args().Get(0)
			p, histRoot, layer, err := resolveConfigScope(c.String("repo"), c.Bool("global"))
			if err != nil {
				return err
			}
			body, removed, err := applyConfigUnset(p, layer, path)
			if err != nil {
				return err
			}
			if err := persistConfigBody(ctx, histRoot, p, body, layer == "global"); err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(map[string]any{
					"repo":    histRoot,
					"file":    p,
					"scope":   layer,
					"unset":   path,
					"removed": removed,
				})
			}
			PrintOK("unset %s (prior content in %s history)", path, layer)
			return nil
		},
	}
}

// applyConfigUnset deletes the key/element at the dotted path and
// returns the new file body plus the removed value rendered as JSON
// ("" when nothing was there). Shares applyConfigSet's read / parse /
// scope-check / validate plumbing; only the patch op differs.
func applyConfigUnset(p, layer, path string) (body []byte, removed string, err error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", p, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", p, err)
	}
	segs, err := yamlpatch.ParsePath(path)
	if err != nil {
		return nil, "", err
	}
	if len(segs) > 0 && segs[0].Key != "" {
		if err := config.CheckKeyInLayer(segs[0].Key, layer); err != nil {
			return nil, "", err
		}
	}
	prev, err := yamlpatch.Unset(&doc, segs)
	if err != nil {
		return nil, "", err
	}
	body, err = yamlpatch.Marshal(&doc)
	if err != nil {
		return nil, "", fmt.Errorf("encode yaml: %w", err)
	}
	var validated config.Config
	if err := yaml.Unmarshal(body, &validated); err != nil {
		return nil, "", fmt.Errorf("validation failed — patched file would not parse as config.Config: %w", err)
	}
	return body, decodePrevJSON(prev), nil
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
			&cli.BoolFlag{Name: "global", Usage: "list generations of the user-global config instead of .treeman.yaml"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			p, histRoot, layer, err := resolveConfigScope(c.String("repo"), c.Bool("global"))
			if err != nil {
				return err
			}
			st, err := openDefaultStore(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			gens, err := st.ListConfigGenerations(ctx, histRoot, p)
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
				return jsonStream(map[string]any{"repo": histRoot, "file": p, "scope": layer, "generations": rows})
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
			&cli.BoolFlag{Name: "global", Usage: "restore a generation of the user-global config instead of .treeman.yaml"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman config restore <generation>")
			}
			gen, err := strconv.ParseInt(c.Args().Get(0), 10, 64)
			if err != nil {
				return fmt.Errorf("generation must be an integer: %w", err)
			}
			p, histRoot, layer, err := resolveConfigScope(c.String("repo"), c.Bool("global"))
			if err != nil {
				return err
			}
			st, err := openDefaultStore(ctx)
			if err != nil {
				return err
			}
			g, err := st.GetConfigGeneration(ctx, histRoot, p, gen)
			_ = st.Close()
			if err != nil {
				return fmt.Errorf("generation %d not found for %s", gen, p)
			}
			if err := config.CheckBodyScope(g.Content, layer); err != nil {
				return fmt.Errorf("generation %d cannot be restored: %w", gen, err)
			}
			if err := persistConfigBody(ctx, histRoot, p, g.Content, layer == "global"); err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(map[string]any{
					"repo":     histRoot,
					"file":     p,
					"scope":    layer,
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
			{
				Name:   "start",
				Usage:  "start treemand in the background (no-op when already running)",
				Action: daemonStart,
			},
			{
				Name:   "stop",
				Usage:  "request a graceful treemand shutdown",
				Action: daemonStop,
			},
			{
				Name:  "reload",
				Usage: "ask the daemon to re-read config + restart watchers (no process restart)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "limit reload to one repo path; defaults to all"},
				},
				Action: daemonReload,
			},
			{
				Name:        "status",
				Usage:       "show whether treemand is running (pid, version, watchers)",
				Description: "Exits 0 whether or not the daemon is up (a stopped daemon is a\nstatus, not an error) — pass --fail-if-down for scripting gates, which\nescalates a missing/mismatched daemon to exit code 3.",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "json"},
					&cli.BoolFlag{
						Name:  "fail-if-down",
						Usage: "exit 3 when the daemon is unreachable (or speaks the wrong protocol) instead of 0",
					},
				},
				Action: daemonStatus,
			},
			{
				Name:   "state",
				Usage:  "live runtime snapshot — watchers, in-flight finalizes/teardowns, auto-fetch backoffs (CLI twin of the MCP daemon_state tool)",
				Flags:  []cli.Flag{&cli.BoolFlag{Name: "json"}},
				Action: daemonState,
			},
			{
				Name:   "install",
				Usage:  "install + enable the treemand auto-start unit (systemd user service on Linux, launchd on macOS)",
				Action: daemonInstall,
			},
			{
				Name:  "uninstall",
				Usage: "remove the treemand auto-start unit",
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
	var pme *rpc.ProtocolMismatchError
	mismatch := errors.As(err, &pme)
	if err != nil && !mismatch && !errors.Is(err, rpc.ErrDaemonUnreachable) {
		return fmt.Errorf("query daemon status: %w", err)
	}
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
		if mismatch {
			out["status"] = "protocol-mismatch"
		}
		if jerr := jsonStream(out); jerr != nil {
			return jerr
		}
		if c.Bool("fail-if-down") && err != nil {
			return cli.Exit("", 3)
		}
		return nil
	}
	if err != nil {
		if mismatch {
			ui.Warn("treemand protocol mismatch")
			ui.Hint("%s", pme.Error())
		} else {
			ui.Warn("daemon not running")
			ui.Hint("start it with: treeman daemon start")
			ui.Hint("or auto-launch on login: treeman daemon install")
		}
		// A stopped daemon is a status, not an error — unless the caller
		// is a script gate that needs a distinct signal. Exit 3, not 1:
		// 1 stays reserved for hard command failures.
		if c.Bool("fail-if-down") {
			return cli.Exit("", 3)
		}
		return nil
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
		Name:  "frameworks",
		Usage: "framework detection",
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
				migs := ui.NewTable("MIGRATION_FW", "ON_MODIFY", "DIRS", "MIGRATE")
				for _, s := range detected {
					migs.Row(ui.Cyan(s.Name), string(s.OnModify), ui.Dim(strings.Join(s.MigrationDirs, ",")), ui.Dim(s.MigrateRun))
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
		Name:      "slug",
		Usage:     "print the slug treeman derives for a worktree",
		ArgsUsage: "[path]",
		Flags:     []cli.Flag{&cli.BoolFlag{Name: "json"}},
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
			&cli.StringFlag{
				Name:  "engine",
				Usage: "comma-separated engines to activate in databases: (e.g. mysql,postgres,redis) — emits minimal blocks when detection found no framework",
			},
			&cli.BoolFlag{
				Name:  "interactive",
				Usage: "pick engines from a terminal picker instead of --engine (declines gracefully when stdin is not a TTY)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Bool("global") {
				return initGlobalAction(c)
			}
			return initRepoAction(c)
		},
	}
}

// initRepoAction scaffolds .treeman.yaml for the repo rooted at cwd:
// detection-driven template, optional --engine/--interactive databases
// activation, immediate load-validation, and the first-run hints.
// Inside a monorepo subdir (#66) it scaffolds a hierarchical fragment
// instead — but only when the root already has a config; a fragment
// without a root config would be silently inert for repo-wide reads,
// so init refuses and points at the root.
func initRepoAction(c *cli.Command) error {
	cwd, _ := os.Getwd()
	if err := guardSubdirInit(cwd); err != nil {
		return err
	}
	detected := framework.DefaultRegistry().DetectAll(cwd)
	engines := enginesForScaffold(c)
	if c.Bool("interactive") && len(engines) == 0 && !tui.Interactive() {
		// A non-interactive --interactive (CI, piped stdin) must
		// not wedge on the picker: degrade to the plain scaffold.
		PrintWarn("--interactive needs a terminal — scaffolding without engine selection")
	}
	if c.Bool("interactive") && tui.Interactive() && len(engines) == 0 {
		var err error
		if engines, err = pickEngines(); err != nil {
			return err
		}
	}
	target, created, body, err := InitTreemanYAML(cwd, c.Bool("force"))
	if err != nil {
		return err
	}
	if len(engines) > 0 {
		if err := initgen.AppendEngines(target, engines); err != nil {
			return err
		}
		if body, err = readFileString(target); err != nil {
			return err
		}
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
			"engines":  engines,
		})
	}
	printInitSummary(detected, engines)
	PrintOK("wrote %s", target)
	PrintHint("review the generated databases:/hooks: blocks before first create")
	// First-run guard (#71): the scaffold must load. A failure
	// here is a template regression — surface it now instead of
	// at the user's first `worktree create`. A subdir fragment loads
	// through the worktree path so the hierarchical merge sees it.
	var loadErr error
	if root, rerr := DiscoverRepoRoot(cwd); rerr == nil && root != cwd {
		_, loadErr = resolve.LoadResolvedForWorktree(root, cwd)
	} else {
		_, loadErr = resolve.LoadResolved(cwd)
	}
	if loadErr != nil {
		return fmt.Errorf("the scaffolded config does not load (report this): %w", loadErr)
	}
	if !daemonAutoStartInstalled() {
		PrintHint("install the daemon (one-time): treeman daemon install")
	}
	PrintHint("create a worktree:               treeman worktree create <branch>")
	PrintHint("install JSON Schema (editors):   treeman schema install")
	PrintHint("shell completions (one-time):    source <(treeman completion zsh)  # or bash/fish/pwsh")
	return nil
}

// printInitSummary reports what detection + engine activation decided.
func printInitSummary(detected []framework.Spec, engines []string) {
	switch {
	case len(detected) > 0:
		names := make([]string, 0, len(detected))
		for _, s := range detected {
			names = append(names, s.Name)
		}
		PrintInfo("detected: %s", strings.Join(names, ", "))
	case len(engines) > 0:
		PrintInfo("activated databases: for %s (--engine)", strings.Join(engines, ", "))
	default:
		PrintWarn("no migration framework detected — the databases: block was left commented out")
		PrintHint("activate one non-interactively:  treeman init --force --engine mysql,postgres")
		PrintHint("or pick from a picker:            treeman init --force --interactive")
	}
}

// guardSubdirInit implements the monorepo-subdir guardrail (#66):
// `init` inside a repo subdir scaffolds a hierarchical fragment, which
// only works when the root config already exists — refuse otherwise,
// pointing at the root.
func guardSubdirInit(cwd string) error {
	root, err := DiscoverRepoRoot(cwd)
	if err != nil {
		root = ""
	}
	if root == "" || root == cwd {
		// Not inside a recognizable repo (or already at its root) — the
		// plain scaffold path handles both, errors included.
		return nil
	}
	if _, statErr := os.Stat(filepath.Join(root, ".treeman.yaml")); statErr != nil {
		rel, _ := filepath.Rel(root, cwd)
		return fmt.Errorf(
			"%s is inside the treeman repo rooted at %s, which has no .treeman.yaml yet — run `treeman init` at the repo root first; a %s fragment only extends the root config",
			cwd,
			root,
			filepath.Join(rel, ".treeman.yaml"),
		)
	}
	rel, _ := filepath.Rel(root, cwd)
	PrintInfo("scaffolding a hierarchical fragment: it applies to worktrees under %s only", rel)
	return nil
}

// enginesForScaffold parses --engine (comma-separated, trimmed).
func enginesForScaffold(c *cli.Command) []string {
	raw := c.String("engine")
	if raw == "" {
		return nil
	}
	var engines []string
	for eng := range strings.SplitSeq(raw, ",") {
		eng = strings.TrimSpace(eng)
		if eng != "" {
			engines = append(engines, eng)
		}
	}
	return engines
}

// pickEngines runs the terminal multi-select over the engine families
// treeman can scaffold. Empty selection = "leave databases commented".
func pickEngines() ([]string, error) {
	items := []string{
		"mysql      — MySQL / MariaDB / TiDB",
		"postgres   — PostgreSQL",
		"mongodb    — MongoDB",
		"redis      — Redis / Valkey / Dragonfly",
		"elasticsearch — Elasticsearch / OpenSearch",
	}
	values := []string{"mysql", "postgres", "mongodb", "redis", "elasticsearch"}
	res, err := tui.Select(items, tui.Options{
		Prompt: "engines to activate in databases: (space marks, enter confirms, esc = none)",
		Values: values,
		Multi:  true,
	})
	switch {
	case errors.Is(err, tui.ErrAborted), errors.Is(err, tui.ErrCanceled):
		return nil, nil
	case err != nil:
		return nil, err
	}
	var engines []string
	for _, idx := range res.Indices {
		if idx >= 0 && idx < len(values) {
			engines = append(engines, values[idx])
		}
	}
	return engines, nil
}

func readFileString(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
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

// detectBranchOfWorktree reads the worktree's HEAD file (fork-free)
// and returns the branch or "".
func detectBranchOfWorktree(worktree string) string {
	return gitenv.DetectBranch(context.Background(), worktree)
}
