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

	"github.com/invopop/jsonschema"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
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
	sl := slug.For(wt, branch)
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	wtID, _ := st.EnsureWorktree(ctx, repoID, wt, sl.Value, branch)
	return prepare.Run(ctx, &cfg, wt, sl, st, repoID, wtID, CaptureInheritedEnv())
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
					return fmt.Errorf("usage: treeman hook run <precreate|postcreate|predelete|postdelete>")
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
				switch phase {
				case "precreate":
					fmt.Printf("precreate: exit=%d (%d steps)\n", out.AggregateExitCode, len(out.Groups))
				default:
					fmt.Printf("%s: %d group(s) complete\n", phase, len(out.Groups))
				}
				return nil
			},
		}},
	}
}

// RunHookPhase is the shared core for `treeman hook run <phase>`
// and the MCP `hook_run` tool. Resolves cfg, runs the phase
// synchronously, and returns the hooks.RunOutcome so callers can
// inspect group exit codes / tails.
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
	sl := slug.For(wt, branch)
	env := CaptureInheritedEnv()
	switch phase {
	case "precreate":
		return hooks.RunPrecreateHooks(ctx, cfg.Hooks.Precreate, repoRoot, wt, sl.Value, env)
	case "postcreate":
		return hooks.RunHooks(ctx, phase, cfg.Hooks.Postcreate, repoRoot, wt, sl.Value, env, true)
	case "predelete":
		return hooks.RunHooks(ctx, phase, cfg.Hooks.Predelete, repoRoot, wt, sl.Value, env, true)
	case "postdelete":
		return hooks.RunHooks(ctx, phase, cfg.Hooks.Postdelete, repoRoot, wt, sl.Value, env, true)
	}
	return hooks.RunOutcome{}, fmt.Errorf("unknown phase: %s", phase)
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
//	dump          → write schema to stdout
//	dump --out P  → write schema to file P
//	install       → write schema to `schemas/treeman.schema.json`
//	                relative to the current repo root + add a
//	                yaml-language-server modeline to .treeman.yaml
//	                if one is missing
func SchemaCmd() *cli.Command {
	return &cli.Command{
		Name:  "schema",
		Usage: "JSON schema helpers",
		Commands: []*cli.Command{
			{
				Name:  "dump",
				Flags: []cli.Flag{&cli.StringFlag{Name: "out"}},
				Action: func(ctx context.Context, c *cli.Command) error {
					b, err := renderConfigSchema()
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
				Action: func(ctx context.Context, c *cli.Command) error {
					cwd, _ := os.Getwd()
					repoRoot, err := DiscoverRepoRoot(cwd)
					if err != nil {
						return err
					}
					b, err := renderConfigSchema()
					if err != nil {
						return err
					}
					dst := filepath.Join(repoRoot, "schemas", "treeman.schema.json")
					if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(dst, b, 0o644); err != nil {
						return err
					}
					PrintOK("wrote %s", dst)
					return nil
				},
			},
		},
	}
}

func renderConfigSchema() ([]byte, error) {
	r := &jsonschema.Reflector{
		Anonymous:      true,
		ExpandedStruct: true,
		FieldNameTag:   "yaml",
	}
	s := r.Reflect(&config.Config{})
	return json.MarshalIndent(s, "", "  ")
}

// DaemonCmd — `treeman daemon {start,stop,status,install,uninstall}`.
func DaemonCmd() *cli.Command {
	return &cli.Command{
		Name:  "daemon",
		Usage: "daemon lifecycle",
		Commands: []*cli.Command{
			{Name: "start", Action: daemonStart},
			{Name: "stop", Action: daemonStop},
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

// startDaemonProcess is daemonStart minus the user-facing PrintOK.
// Returns the spawned PID when we forked the binary directly; zero
// when handed off to systemd / launchd (no pid known at this level).
// Used by `daemon start` AND by the auto-recover paths in wt
// create / wt delete that call `ensureDaemon` before falling back to
// a detached local finalize.
func startDaemonProcess(ctx context.Context) (int, error) {
	// Prefer the OS-native init when its unit is installed; else
	// spawn the binary detached.
	switch runtime.GOOS {
	case "darwin":
		uid := os.Getuid()
		domain := fmt.Sprintf("gui/%d", uid)
		if err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", domain+"/"+launchdLabel).Run(); err == nil {
			return 0, nil
		}
	default:
		if err := exec.CommandContext(ctx, "systemctl", "--user", "is-enabled", "treemand").Run(); err == nil {
			return 0, exec.CommandContext(ctx, "systemctl", "--user", "start", "treemand").Run()
		}
	}

	binPath, err := exec.LookPath("treemand")
	if err != nil {
		return 0, fmt.Errorf("treemand not on PATH: %w", err)
	}
	cmd := exec.Command(binPath)
	cmd.Stdin = nil
	cmd.Stdout, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func daemonStop(ctx context.Context, c *cli.Command) error {
	switch runtime.GOOS {
	case "darwin":
		uid := os.Getuid()
		domain := fmt.Sprintf("gui/%d", uid)
		if err := exec.CommandContext(ctx, "launchctl", "kill", "TERM", domain+"/"+launchdLabel).Run(); err == nil {
			return nil
		}
	default:
		if err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "treemand").Run(); err == nil {
			return exec.CommandContext(ctx, "systemctl", "--user", "stop", "treemand").Run()
		}
	}
	// Fallback: shutdown RPC.
	resp, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodShutdown})
	if err != nil {
		return err
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
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

const launchdLabel = "dev.stubbe.treemand"

func daemonInstallLaunchd(ctx context.Context) error {
	bin := mustResolveTreemand()
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".local", "share", "treeman")
	_ = os.MkdirAll(logDir, 0o755)
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>            <string>` + launchdLabel + `</string>
    <key>ProgramArguments</key> <array><string>` + bin + `</string></array>
    <key>RunAtLoad</key>        <true/>
    <key>KeepAlive</key>        <true/>
    <key>StandardOutPath</key>  <string>` + filepath.Join(logDir, "treemand.log") + `</string>
    <key>StandardErrorPath</key><string>` + filepath.Join(logDir, "treemand.log") + `</string>
    <key>ProcessType</key>      <string>Background</string>
</dict>
</plist>
`
	dst := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
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
	if err := exec.CommandContext(ctx, "launchctl", "enable", domain+"/"+launchdLabel).Run(); err != nil {
		return fmt.Errorf("launchctl enable %s: %w", launchdLabel, err)
	}
	PrintOK("installed + enabled %s at %s", launchdLabel, dst)
	return nil
}

func daemonUninstallLaunchd(ctx context.Context) error {
	home, _ := os.UserHomeDir()
	dst := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain, dst).Run()
	_ = os.Remove(dst)
	PrintOK("uninstalled %s", launchdLabel)
	return nil
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
			PrintHint("install the daemon (one-time): treeman daemon install")
			PrintHint("create a worktree:               treeman wt create <branch>")
			PrintHint("install JSON Schema (editors):   treeman schema install")
			return nil
		},
	}
}

// InitTreemanYAML scaffolds a `.treeman.yaml` under cwd. Returns the
// absolute target path, whether a new file was created (false when
// the file already existed and force was true), and the body bytes.
// Shared by the CLI and the MCP `init_repo` tool.
func InitTreemanYAML(cwd string, force bool) (path string, created bool, body string, err error) {
	target := filepath.Join(cwd, ".treeman.yaml")
	_, statErr := os.Stat(target)
	exists := statErr == nil
	if exists && !force {
		return target, false, "", fmt.Errorf("%s already exists (pass force to overwrite)", target)
	}
	body = renderInitTemplate(cwd)
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		return target, false, "", err
	}
	return target, !exists, body, nil
}

// renderInitTemplate inspects `cwd` and emits a fully-declarative
// `.treeman.yaml`. Detection drives WHAT to emit; the runtime never
// re-detects — every directive treeman acts on at runtime lives in
// the YAML this function writes.
//
// Pulled out into a pure function so it can be exercised in unit
// tests against fixture trees.
func renderInitTemplate(cwd string) string {
	name := filepath.Base(cwd)
	has := func(p string) bool {
		_, err := os.Stat(filepath.Join(cwd, p))
		return err == nil
	}

	jsPkgMgr := detectJSPkgMgr(cwd)
	hasGoMod := has("go.mod")
	hasComposer := has("composer.json")
	frontendDir := ""
	for _, candidate := range []string{"frontend", "resources/js", "client", "ui"} {
		if has(filepath.Join(candidate, "package.json")) {
			frontendDir = candidate
			break
		}
	}

	// Pick the first matching built-in framework preset so init can
	// emit its dirs/globs/lockfiles verbatim. If no preset matches,
	// the databases: block is commented out so the user can author
	// it by hand.
	detected := framework.DefaultRegistry().DetectAll(cwd)

	var b strings.Builder
	b.WriteString("# yaml-language-server: $schema=https://raw.githubusercontent.com/stubbedev/treeman/master/schemas/treeman.schema.json\n")
	fmt.Fprintf(&b, "repo:\n  name: %s\n", name)
	b.WriteString("worktrees:\n  root: .worktrees\n  links:\n    - .env\n")

	envSources := defaultEnvSourcesFor(detected, has)
	if len(envSources) > 0 {
		b.WriteString("env_scoping:\n")
		b.WriteString("  sources:        # files read in order, last wins\n")
		for _, s := range envSources {
			fmt.Fprintf(&b, "    - %s\n", s)
		}
		// Only Laravel's .env.testing is conventionally patched-in-
		// place; for anything else the user can extend the list.
		if hasFrameworkNamed(detected, "laravel") {
			b.WriteString("  files: [\".env.testing\"]\n")
			b.WriteString("  skip_worktree: true\n")
			b.WriteString("  patches:\n")
			fmt.Fprintf(&b, "    - { key: DB_TEST_DATABASE, template: \"%s_testing_{slug}\" }\n", name)
		}
	}

	if len(detected) > 0 {
		spec := detected[0]
		engine := spec.EngineHint
		if engine == "" {
			engine = "mysql"
		}
		b.WriteString("databases:\n")
		fmt.Fprintf(&b, "  - engine: %s\n", engine)
		fmt.Fprintf(&b, "    name_template: \"%s_testing_{slug}\"\n", name)
		b.WriteString("    migrations:\n")
		fmt.Fprintf(&b, "      framework: %s        # label only — runtime uses the fields below\n", spec.Name)
		emitStringList(&b, "      ", "migration_dirs", spec.MigrationDirs)
		emitStringList(&b, "      ", "file_globs", spec.FileGlobs)
		emitStringList(&b, "      ", "lockfiles", spec.Lockfiles)
		fmt.Fprintf(&b, "      hash_mode: %s\n", spec.HashMode)
		fmt.Fprintf(&b, "      on_modify: %s\n", spec.OnModify)
		b.WriteString("    test_clones:\n")
		b.WriteString("      clones: auto\n")
		fmt.Fprintf(&b, "      name_template: \"%s_testing_{slug}_test_{n}\"\n", name)
	} else {
		b.WriteString("# databases: []   # add an engine block when DB scaffolding is needed.\n")
	}

	b.WriteString("hooks:\n  postcreate:\n")
	groupsEmitted := 0
	if hasComposer {
		b.WriteString("    - group:\n        - composer install --no-interaction --prefer-dist\n")
		groupsEmitted++
	}
	if jsPkgMgr != "" {
		fmt.Fprintf(&b, "    - group:\n        - %s\n", jsInstallCmd(jsPkgMgr))
		groupsEmitted++
	}
	if frontendDir != "" && jsPkgMgr != "" {
		fmt.Fprintf(&b, "    - group:\n        - cd %s && %s\n", frontendDir, jsInstallCmd(jsPkgMgr))
		groupsEmitted++
	}
	if hasGoMod {
		b.WriteString("    - group:\n        - go mod download\n")
		groupsEmitted++
	}
	if groupsEmitted == 0 {
		b.WriteString("    # add install commands here — each `group` runs in parallel;\n")
		b.WriteString("    # entries within a group run in sequence.\n")
		b.WriteString("    []\n")
	}
	return b.String()
}

// defaultEnvSourcesFor returns a sensible (but explicit) env source
// list based on detected frameworks + which files actually exist
// on disk. The user is expected to edit the list; treeman never
// adds defaults at runtime.
func defaultEnvSourcesFor(detected []framework.Spec, has func(string) bool) []string {
	candidates := []string{".env", ".env.local"}
	if hasFrameworkNamed(detected, "laravel") {
		candidates = append(candidates, ".env.testing", ".env.testing.local")
	} else if hasFrameworkNamed(detected, "rails", "django") {
		candidates = append(candidates, ".env.test", ".env.test.local")
	}
	var out []string
	for _, c := range candidates {
		if has(c) {
			out = append(out, c)
		}
	}
	// Even if none exist yet, emit the conventional baseline so the
	// user has a list to extend.
	if len(out) == 0 {
		out = candidates[:1]
	}
	return out
}

func hasFrameworkNamed(specs []framework.Spec, names ...string) bool {
	for _, s := range specs {
		for _, n := range names {
			if s.Name == n {
				return true
			}
		}
	}
	return false
}

func emitStringList(b *strings.Builder, indent, key string, vals []string) {
	if len(vals) == 0 {
		fmt.Fprintf(b, "%s%s: []\n", indent, key)
		return
	}
	fmt.Fprintf(b, "%s%s:\n", indent, key)
	for _, v := range vals {
		fmt.Fprintf(b, "%s  - %q\n", indent, v)
	}
}

// detectJSPkgMgr returns "yarn", "pnpm", "npm", "bun", or "deno"
// based on lockfile presence — or "" when there's no package.json.
// Precedence matches the lockfile that pins the toolchain version
// (deno.lock first, then bun.lockb, then pnpm-lock.yaml, then
// yarn.lock, then package-lock.json) so the highest-fidelity install
// wins.
func detectJSPkgMgr(cwd string) string {
	has := func(p string) bool {
		_, err := os.Stat(filepath.Join(cwd, p))
		return err == nil
	}
	switch {
	case has("deno.lock") || has("deno.json"):
		return "deno"
	case has("bun.lockb"):
		return "bun"
	case has("pnpm-lock.yaml"):
		return "pnpm"
	case has("yarn.lock"):
		return "yarn"
	case has("package-lock.json"):
		return "npm"
	case has("package.json"):
		// package.json without a lockfile — fall back to npm.
		return "npm"
	}
	return ""
}

func jsInstallCmd(mgr string) string {
	switch mgr {
	case "yarn":
		return "yarn install --frozen-lockfile"
	case "pnpm":
		return "pnpm install --frozen-lockfile"
	case "bun":
		return "bun install --frozen-lockfile"
	case "deno":
		return "deno install --lock=deno.lock"
	case "npm":
		return "npm ci"
	}
	return "npm ci"
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
