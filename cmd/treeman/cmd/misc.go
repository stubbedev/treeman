package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemonctl"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/initgen"
	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
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
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			outs, err := RunPrepareOnWorktree(ctx, c.String("worktree"), c.String("repo"))
			if err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(map[string]any{"outcomes": outs})
			}
			for _, o := range outs {
				fmt.Printf("[%s] %s template=%s clones=%d\n", o.Engine, o.SourceDB, o.TemplateName, len(o.Clones))
			}
			return nil
		},
	}
}

// RunPrepareOnWorktree is the shared core for `treeman prepare` and
// the MCP `prepare_run` tool. Discovers the worktree + repo root,
// loads resolved config, opens the SQLite store, and dispatches
// prepare.Run. Returns the per-engine outcomes so callers can render
// them however they like.
func RunPrepareOnWorktree(ctx context.Context, worktree, repoOverride string) ([]prepare.Outcome, error) {
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
	defer st.Close()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	// Route through ResolveIdentity so this manual path matches the daemon:
	// the main-worktree overlay is applied (bare active DB name) and a linked
	// worktree's slug is its branch-independent path slug.
	id, err := wt2.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
	if err != nil {
		return nil, err
	}
	return prepare.Run(ctx, &cfg, wt, id.Slug, st, repoID, id.WtID, CaptureInheritedEnv())
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
					&cli.StringFlag{Name: "engine", Usage: "restrict the reset to one engine family (mysql, postgres, mongodb, redis, elasticsearch; aliases like mariadb/postgresql accepted)"},
					&cli.BoolFlag{Name: "json"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					wt := c.Args().First()
					outs, err := RunDbResetOnWorktree(ctx, wt, c.String("repo"), strings.ToLower(c.String("engine")))
					if err != nil {
						return err
					}
					if c.Bool("json") {
						return jsonStream(map[string]any{"outcomes": outs})
					}
					for _, o := range outs {
						fmt.Printf("[%s] %s re-seeded from base\n", o.Engine, o.SourceDB)
					}
					if len(outs) == 0 {
						PrintInfo("no branch_scoped databases configured")
					}
					return nil
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
							active = "(none)"
						}
						fmt.Printf("[%s] %s — active branch: %s; resumable: %s\n",
							d.Engine, d.Active, active, strings.Join(d.ResumableBranches, ", "))
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
	defer st.Close()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	id, err := wt2.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
	if err != nil {
		return nil, err
	}
	return prepare.BranchScopedStatus(ctx, &cfg, repoRoot, wt, id.WtID, st)
}

// RunDbResetOnWorktree resolves the worktree + repo, drops every
// branch_scoped database's active namespace + current durable copy,
// then re-runs prepare so each is repopulated from the live base branch.
func RunDbResetOnWorktree(ctx context.Context, worktree, repoOverride, engineFilter string) ([]prepare.Outcome, error) {
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
	defer st.Close()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	// Route through ResolveIdentity so the main-worktree overlay is applied
	// (bare active DB name) and the slug matches the daemon's branch-
	// independent value — reset operates on the same active namespace the
	// swap lifecycle created.
	id, err := wt2.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
	if err != nil {
		return nil, err
	}
	if err := prepare.ResetBranchScoped(ctx, &cfg, wt, repoID, id.WtID, st, engineFilter); err != nil {
		return nil, err
	}
	outs, err := prepare.Run(ctx, &cfg, wt, id.Slug, st, repoID, id.WtID, CaptureInheritedEnv())
	if err != nil {
		return outs, err
	}
	// Surface only the seeded databases the reset actually touched.
	// Match on the canonical engine family so an alias (mariadb, tidb,
	// postgresql, opensearch) lines up with the canonical Outcome.Engine
	// and an `--engine` filter written as the family name still hits.
	filterLabel := engineFilter
	if engineFilter != "" {
		if fam, ok := engine.Canonical(engineFilter); ok {
			filterLabel = string(fam)
		}
	}
	var seeded []prepare.Outcome
	for _, o := range outs {
		for _, d := range cfg.Databases {
			if !d.BranchScoped {
				continue
			}
			fam, ok := engine.Canonical(d.Engine)
			if !ok {
				continue
			}
			label := string(fam)
			if filterLabel != "" && label != filterLabel {
				continue
			}
			if label == o.Engine {
				seeded = append(seeded, o)
				break
			}
		}
	}
	return seeded, nil
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
			},
			Action: func(ctx context.Context, c *cli.Command) error {
				if c.NArg() < 1 {
					return fmt.Errorf("usage: treeman hook run <setup|teardown>")
				}
				phase := c.Args().First()
				out, err := RunHookPhase(ctx, phase, c.String("worktree"))
				if err != nil {
					return err
				}
				if c.Bool("json") {
					return jsonStream(map[string]any{
						"phase":   phase,
						"outcome": out,
					})
				}
				fmt.Printf("%s: %d action(s) complete\n", phase, len(out.Groups))
				return nil
			},
		}},
	}
}

// RunHookPhase is the shared core for `treeman hook run <phase>`
// and the MCP `hook_run` tool. Resolves cfg, runs the phase
// synchronously, and returns the hooks.RunOutcome so callers can
// inspect group exit codes / tails. Each run also persists a
// hook_runs row + emits hook_start/hook_done events so the operation
// is later searchable from `treeman logs`.
func RunHookPhase(ctx context.Context, phase, worktree string) (hooks.RunOutcome, error) {
	wt := worktree
	if wt == "" {
		cwd, _ := os.Getwd()
		wt = cwd
	}
	wt = MustAbs(wt)
	repoRoot, err := DiscoverRepoRoot(wt)
	if err != nil {
		return hooks.RunOutcome{}, err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return hooks.RunOutcome{}, err
	}
	branch := detectBranchOfWorktree(wt)
	env := CaptureInheritedEnv()

	var (
		st           *store.Store
		repoID, wtID int64
		sl           slug.Slug
		isMain       bool
		entries      int
		startedMs    int64
		out          hooks.RunOutcome
		runErr       error
	)
	dbPath, _ := store.DefaultDBPath()
	if dbPath != "" {
		if s, oerr := store.Open(ctx, dbPath); oerr == nil {
			st = s
			defer st.Close()
			repoID, _ = st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
			id, idErr := wt2.ResolveIdentity(ctx, st, &cfg, repoRoot, wt, branch, repoID)
			if idErr != nil {
				return hooks.RunOutcome{}, idErr
			}
			sl, wtID, isMain = id.Slug, id.WtID, id.IsMain
		}
	}
	if sl.Value == "" {
		// Store unreachable — fall back to a path-hash slug so the hook
		// env still has something. $TREEMAN_IS_MAIN reads "0".
		sl = slug.For(wt, branch)
	}

	var hookEntries []config.Action
	switch phase {
	case "on-create-before-engines":
		hookEntries = cfg.Hooks.OnCreateBeforeEngines
	case "on-create-after-engines":
		hookEntries = cfg.Hooks.OnCreateAfterEngines
	case "on-delete-before-engines":
		hookEntries = cfg.Hooks.OnDeleteBeforeEngines
	case "on-delete-after-engines":
		hookEntries = cfg.Hooks.OnDeleteAfterEngines
	case "on-checkout":
		hookEntries = cfg.Hooks.OnCheckout
	default:
		return hooks.RunOutcome{}, fmt.Errorf("unknown phase: %s (want on-create-before-engines|on-create-after-engines|on-delete-before-engines|on-delete-after-engines|on-checkout)", phase)
	}
	entries = len(hookEntries)

	startedMs = hooks.EmitHookStart(ctx, st, repoID, wtID, phase, entries)
	out, runErr = hooks.RunHooks(ctx, phase, hookEntries, repoRoot, wt, sl.Value, isMain, env, true)
	hooks.PersistOutcome(ctx, st, repoID, wtID, phase, startedMs, time.Now().UnixMilli(), out)
	return out, runErr
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
					fmt.Fprint(ui.Out, string(b))
					if c.Bool("resolved") {
						r := resolve.Resolve(&cfg, repoRoot)
						fmt.Fprintln(ui.Out)
						fmt.Fprintln(ui.Out, "# resolved connections")
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
		},
	}
}

func printResolved(name string, c any) {
	// `c` is a pointer to a resolvedConn[…] struct from
	// internal/resolve. We only need a string representation; JSON
	// is good enough. Write through ui.Out so the pager wrap in
	// `config show` captures these lines too.
	if isNil(c) {
		fmt.Fprintf(ui.Out, "# %s <- (none)\n", name)
		return
	}
	b, _ := json.Marshal(c)
	fmt.Fprintf(ui.Out, "# %s <- %s\n", name, string(b))
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case interface{ IsNil() bool }:
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
				Name:  "dump",
				Flags: []cli.Flag{&cli.StringFlag{Name: "out"}},
				Action: func(ctx context.Context, c *cli.Command) error {
					b, err := schema.Render()
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
		return fmt.Errorf("--global and --url are mutually exclusive")
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
		PrintOK("updated modeline in .treeman.yaml → %s", resolved)
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
		return nil
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
			return fmt.Errorf("aborted")
		}
	}
	switch runtime.GOOS {
	case "darwin":
		return daemonUninstallLaunchd(ctx)
	default:
		return daemonUninstallSystemd(ctx)
	}
}

func daemonInstallSystemd(ctx context.Context) error {
	unit := `[Install]
WantedBy=default.target

[Service]
ExecStart=` + mustResolveTreemand() + `
Restart=on-failure
RestartSec=2
Type=simple

[Unit]
After=default.target
Description=Treeman per-worktree DB orchestrator daemon
`
	home, _ := os.UserHomeDir()
	dst := filepath.Join(home, ".config", "systemd", "user", "treemand.service")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run(); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "systemctl", "--user", "enable", "--now", "treemand").Run(); err != nil {
		return err
	}
	PrintOK("installed + enabled treemand.service at %s", dst)
	return nil
}

func daemonUninstallSystemd(ctx context.Context) error {
	_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", "treemand").Run()
	home, _ := os.UserHomeDir()
	dst := filepath.Join(home, ".config", "systemd", "user", "treemand.service")
	_ = os.Remove(dst)
	_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
	PrintOK("uninstalled treemand.service")
	return nil
}

func daemonInstallLaunchd(ctx context.Context) error {
	bin := mustResolveTreemand()
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".local", "share", "treeman")
	_ = os.MkdirAll(logDir, 0o755)
	// KeepAlive is a dict — `SuccessfulExit=false` keeps launchd from
	// respawning the daemon after `treeman daemon stop` (which exits
	// cleanly via the shutdown RPC). Crash exits (non-zero) still
	// trigger relaunch. ThrottleInterval keeps a crash-loop from
	// pegging the CPU while a misconfiguration drives the daemon to
	// die on boot.
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>            <string>` + daemonctl.LaunchdLabel + `</string>
    <key>ProgramArguments</key> <array><string>` + bin + `</string></array>
    <key>RunAtLoad</key>        <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key> <integer>5</integer>
    <key>StandardOutPath</key>  <string>` + filepath.Join(logDir, "treemand.log") + `</string>
    <key>StandardErrorPath</key><string>` + filepath.Join(logDir, "treemand.log") + `</string>
    <key>ProcessType</key>      <string>Background</string>
</dict>
</plist>
`
	dst := filepath.Join(home, "Library", "LaunchAgents", daemonctl.LaunchdLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, []byte(plist), 0o644); err != nil {
		return err
	}
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)
	// Best-effort unload first so re-install doesn't error.
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain, dst).Run()
	if err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, dst).Run(); err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w", dst, err)
	}
	if err := exec.CommandContext(ctx, "launchctl", "enable", domain+"/"+daemonctl.LaunchdLabel).Run(); err != nil {
		return fmt.Errorf("launchctl enable %s: %w", daemonctl.LaunchdLabel, err)
	}
	PrintOK("installed + enabled %s at %s", daemonctl.LaunchdLabel, dst)
	return nil
}

func daemonUninstallLaunchd(ctx context.Context) error {
	home, _ := os.UserHomeDir()
	dst := filepath.Join(home, "Library", "LaunchAgents", daemonctl.LaunchdLabel+".plist")
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain, dst).Run()
	_ = os.Remove(dst)
	PrintOK("uninstalled %s", daemonctl.LaunchdLabel)
	return nil
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

func mustResolveTreemand() string {
	if p, err := exec.LookPath("treemand"); err == nil {
		return p
	}
	return "/usr/local/bin/treemand"
}

// FwCmd — `treeman fw detect` lists detected migration + test
// frameworks.
func FwCmd() *cli.Command {
	return &cli.Command{
		Name:    "fw",
		Aliases: []string{"frameworks"},
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
				r := framework.DefaultRegistry()
				detected := r.DetectAll(repoRoot)
				if c.Bool("json") {
					return jsonStream(map[string]any{
						"repo":              repoRoot,
						"migration":         detected,
						"test":              testfw.DetectAll(repoRoot),
						"auto_clone_target": testfw.DetectedCloneCount(repoRoot),
					})
				}
				migs := ui.NewTable("MIGRATION_FW", "HASH_MODE", "ON_MODIFY", "DIRS")
				for _, s := range detected {
					migs.Row(ui.Cyan(s.Name), string(s.HashMode), string(s.OnModify), ui.Dim(strings.Join(s.MigrationDirs, ",")))
				}
				if len(detected) == 0 {
					ui.Info("no migration framework detected in %s", repoRoot)
				} else {
					migs.Render(nil)
				}
				fmt.Println()
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
				fmt.Printf("\nauto-clones (replication target): %s\n", ui.Bold(fmt.Sprintf("%d", testfw.DetectedCloneCount(repoRoot))))
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
		},
		Action: func(ctx context.Context, c *cli.Command) error {
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
	if strings.HasPrefix(s, pfx) {
		return strings.TrimPrefix(s, pfx)
	}
	return ""
}
