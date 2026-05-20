package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
)

// PrepareCmd — `treeman prepare` runs the full pipeline foreground.
func PrepareCmd() *cli.Command {
	return &cli.Command{
		Name:  "prepare",
		Usage: "ensure → dump → migrate → snapshot → replicate (foreground)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "worktree", Aliases: []string{"w"}},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			wt := c.String("worktree")
			if wt == "" {
				cwd, _ := os.Getwd()
				wt = cwd
			}
			wt = MustAbs(wt)
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				repoRoot, err = DiscoverRepoRoot(wt)
				if err != nil {
					return err
				}
			}
			cfg, err := config.LoadLayered(repoRoot)
			if err != nil {
				return err
			}
			branch := detectBranchOfWorktree(wt)
			sl := slug.For(wt, branch)
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
			wtID, _ := st.EnsureWorktree(ctx, repoID, wt, sl.Value, branch)
			outs, err := prepare.Run(ctx, &cfg, repoRoot, wt, sl, st, repoID, wtID, CaptureInheritedEnv())
			if err != nil {
				return err
			}
			for _, o := range outs {
				fmt.Printf("[%s] %s template=%s clones=%d\n", o.Engine, o.SourceDB, o.TemplateName, len(o.Clones))
			}
			return nil
		},
	}
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
			Flags: []cli.Flag{&cli.StringFlag{Name: "worktree", Aliases: []string{"w"}}},
			Action: func(ctx context.Context, c *cli.Command) error {
				if c.NArg() < 1 {
					return fmt.Errorf("usage: treeman hook run <precreate|postcreate|predelete|postdelete>")
				}
				phase := c.Args().First()
				wt := c.String("worktree")
				if wt == "" {
					cwd, _ := os.Getwd()
					wt = cwd
				}
				wt = MustAbs(wt)
				repoRoot, err := DiscoverRepoRoot(wt)
				if err != nil {
					return err
				}
				cfg, err := config.LoadLayered(repoRoot)
				if err != nil {
					return err
				}
				branch := detectBranchOfWorktree(wt)
				sl := slug.For(wt, branch)
				env := CaptureInheritedEnv()
				switch phase {
				case "precreate":
					out, err := hooks.RunPrecreateHooks(ctx, cfg.Hooks.Precreate, repoRoot, wt, sl.Value, env)
					if err != nil {
						return err
					}
					fmt.Printf("precreate: exit=%d (%d steps)\n", out.AggregateExitCode, len(out.Groups))
				case "postcreate":
					_, err := hooks.RunHooks(ctx, phase, cfg.Hooks.Postcreate, repoRoot, wt, sl.Value, env)
					if err != nil {
						return err
					}
					fmt.Printf("%s: %d group(s) spawned\n", phase, len(cfg.Hooks.Postcreate))
				case "predelete":
					_, err := hooks.RunHooks(ctx, phase, cfg.Hooks.Predelete, repoRoot, wt, sl.Value, env)
					if err != nil {
						return err
					}
					fmt.Printf("%s: %d group(s) spawned\n", phase, len(cfg.Hooks.Predelete))
				case "postdelete":
					_, err := hooks.RunHooks(ctx, phase, cfg.Hooks.Postdelete, repoRoot, wt, sl.Value, env)
					if err != nil {
						return err
					}
					fmt.Printf("%s: %d group(s) spawned\n", phase, len(cfg.Hooks.Postdelete))
				default:
					return fmt.Errorf("unknown phase: %s", phase)
				}
				return nil
			},
		}},
	}
}

// LogsCmd — `treeman logs {tail,grep}` queries the SQLite events.
func LogsCmd() *cli.Command {
	return &cli.Command{
		Name:    "logs",
		Aliases: []string{"log"},
		Usage:   "query the SQLite event log",
		Commands: []*cli.Command{
			{
				Name:  "tail",
				Usage: "print the most recent N events (default 50)",
				Flags: []cli.Flag{&cli.IntFlag{Name: "n", Value: 50}},
				Action: func(ctx context.Context, c *cli.Command) error {
					return tailLogs(ctx, int(c.Int("n")), "")
				},
			},
			{
				Name:  "grep",
				Usage: "print events whose message matches a substring",
				Action: func(ctx context.Context, c *cli.Command) error {
					if c.NArg() < 1 {
						return fmt.Errorf("usage: treeman logs grep <substring>")
					}
					return tailLogs(ctx, 500, c.Args().First())
				},
			},
		},
	}
}

func tailLogs(ctx context.Context, n int, grep string) error {
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	q := `SELECT ts, level, event_type, COALESCE(message,''), payload_json FROM events`
	args := []any{}
	if grep != "" {
		q += ` WHERE message LIKE ?`
		args = append(args, "%"+grep+"%")
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, n)
	rows, err := st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		ts                          int64
		level, etype, msg, payload  string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ts, &r.level, &r.etype, &r.msg, &r.payload); err != nil {
			return err
		}
		all = append(all, r)
	}
	// Reverse so oldest first (cheap visual order).
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	for _, r := range all {
		fmt.Printf("%s %-5s %-22s %s\n", formatTs(r.ts), r.level, r.etype, r.msg)
	}
	return nil
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
				Action: func(ctx context.Context, c *cli.Command) error {
					repoRoot, err := resolveRepo("")
					if err != nil {
						return err
					}
					cfg, err := config.LoadLayered(repoRoot)
					if err != nil {
						return err
					}
					PrintOK("config loaded (%d databases configured)", len(cfg.Databases))
					return nil
				},
			},
			{
				Name:  "show",
				Usage: "dump the resolved config",
				Flags: []cli.Flag{&cli.BoolFlag{Name: "resolved"}},
				Action: func(ctx context.Context, c *cli.Command) error {
					repoRoot, err := resolveRepo("")
					if err != nil {
						return err
					}
					cfg, err := config.LoadLayered(repoRoot)
					if err != nil {
						return err
					}
					b, _ := yaml.Marshal(cfg)
					fmt.Print(string(b))
					if c.Bool("resolved") {
						r := resolve.Resolve(&cfg, repoRoot)
						fmt.Println()
						fmt.Println("# resolved connections")
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
	// is good enough.
	if isNil(c) {
		fmt.Printf("# %s <- (none)\n", name)
		return
	}
	b, _ := json.Marshal(c)
	fmt.Printf("# %s <- %s\n", name, string(b))
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

// SchemaCmd — `treeman schema {dump,install}`. Minimal stubs.
func SchemaCmd() *cli.Command {
	return &cli.Command{
		Name:  "schema",
		Usage: "JSON schema helpers",
		Commands: []*cli.Command{
			{Name: "dump", Action: func(ctx context.Context, c *cli.Command) error {
				fmt.Println(`{"comment":"JSON schema generation pending — see plan §10."}`)
				return nil
			}},
			{Name: "install", Action: func(ctx context.Context, c *cli.Command) error {
				PrintWarn("schema install: pending — JSON schema generator is planned but not yet wired")
				return nil
			}},
		},
	}
}

// DaemonCmd — `treeman daemon {start,stop,status,install,uninstall}`.
func DaemonCmd() *cli.Command {
	return &cli.Command{
		Name:  "daemon",
		Usage: "daemon lifecycle",
		Commands: []*cli.Command{
			{Name: "start", Action: daemonStart},
			{Name: "stop", Action: daemonStop},
			{Name: "status", Action: daemonStatus},
			{Name: "install", Action: daemonInstall},
			{Name: "uninstall", Action: daemonUninstall},
		},
	}
}

func daemonStart(ctx context.Context, c *cli.Command) error {
	// Prefer systemctl when the unit is installed; else spawn the
	// binary detached.
	if err := exec.CommandContext(ctx, "systemctl", "--user", "is-enabled", "treemand").Run(); err == nil {
		return exec.CommandContext(ctx, "systemctl", "--user", "start", "treemand").Run()
	}
	binPath, err := exec.LookPath("treemand")
	if err != nil {
		return fmt.Errorf("treemand not on PATH: %w", err)
	}
	cmd := exec.Command(binPath)
	cmd.Stdin = nil
	cmd.Stdout, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	PrintOK("treemand started (pid %d)", cmd.Process.Pid)
	return nil
}

func daemonStop(ctx context.Context, c *cli.Command) error {
	if err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "treemand").Run(); err == nil {
		return exec.CommandContext(ctx, "systemctl", "--user", "stop", "treemand").Run()
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
	if err != nil {
		fmt.Println("not running")
		return nil
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	fmt.Printf("treemand %s, pid %d, %d watcher(s)\n", resp.DaemonVersion, resp.Pid, resp.WatcherCount)
	return nil
}

func daemonInstall(ctx context.Context, c *cli.Command) error {
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

func daemonUninstall(ctx context.Context, c *cli.Command) error {
	_ = exec.CommandContext(ctx, "systemctl", "--user", "disable", "--now", "treemand").Run()
	home, _ := os.UserHomeDir()
	dst := filepath.Join(home, ".config", "systemd", "user", "treemand.service")
	_ = os.Remove(dst)
	_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()
	PrintOK("uninstalled treemand.service")
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
			Name: "detect",
			Action: func(ctx context.Context, c *cli.Command) error {
				cwd, _ := os.Getwd()
				repoRoot, err := DiscoverRepoRoot(cwd)
				if err != nil {
					return err
				}
				r := framework.DefaultRegistry()
				detected := r.DetectAll(repoRoot)
				fmt.Printf("%-20s %-14s %-10s %s\n", "MIGRATION_FW", "HASH_MODE", "ON_MODIFY", "DIRS")
				for _, s := range detected {
					fmt.Printf("%-20s %-14s %-10s %s\n", s.Name, s.HashMode, s.OnModify, strings.Join(s.MigrationDirs, ","))
				}
				fmt.Println()
				fmt.Printf("%-22s %-10s %-14s %-10s %s\n", "TEST_FW", "LANGUAGE", "STRATEGY", "WORKER_IDX", "WORKER_ENV")
				for _, t := range testfw.DetectAll(repoRoot) {
					fmt.Printf("%-22s %-10s %-14s %-10s %s\n",
						t.Name, t.Language, t.Strategy, t.WorkerIndex, t.WorkerEnv)
				}
				fmt.Printf("\nauto-clones (replication target): %d\n", testfw.DetectedCloneCount(repoRoot))
				return nil
			},
		}},
	}
}

// SlugCmd — `treeman slug [path]` prints the slug of a worktree.
func SlugCmd() *cli.Command {
	return &cli.Command{
		Name: "slug",
		Action: func(ctx context.Context, c *cli.Command) error {
			path := "."
			if c.NArg() >= 1 {
				path = c.Args().First()
			}
			path = MustAbs(path)
			branch := detectBranchOfWorktree(path)
			sl := slug.For(path, branch)
			fmt.Println(sl.Value)
			return nil
		},
	}
}

// InitCmd — `treeman init` scaffolds a minimal .treeman.yaml. The
// Rust impl had a Laravel preset; we re-use the same shape.
func InitCmd() *cli.Command {
	return &cli.Command{
		Name: "init",
		Flags: []cli.Flag{&cli.BoolFlag{Name: "force"}},
		Action: func(ctx context.Context, c *cli.Command) error {
			cwd, _ := os.Getwd()
			target := filepath.Join(cwd, ".treeman.yaml")
			if _, err := os.Stat(target); err == nil && !c.Bool("force") {
				return fmt.Errorf("%s already exists (pass --force to overwrite)", target)
			}
			body := `repo:
  name: ` + filepath.Base(cwd) + `
worktrees:
  root: .worktrees
  links:
    - .env
env_scoping:
  files: [".env.testing"]
  skip_worktree: true
  patches:
    - { key: DB_TEST_DATABASE, template: "` + filepath.Base(cwd) + `_testing_{slug}" }
databases:
  - engine: mysql
    name_template: "` + filepath.Base(cwd) + `_testing_{slug}"
    paratest:
      clones: auto
      name_template: "` + filepath.Base(cwd) + `_testing_{slug}_test_{n}"
hooks:
  postcreate: []
`
			if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
				return err
			}
			PrintOK("wrote %s", target)
			return nil
		},
	}
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
