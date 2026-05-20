package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/patcher"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
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
			wtFinalize(),
		},
	}
}

func wtCreate() *cli.Command {
	return &cli.Command{
		Name:    "create",
		Aliases: []string{"new"},
		Usage:   "create a worktree end-to-end",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "base branch"},
			&cli.StringFlag{Name: "path", Usage: "explicit worktree path"},
			&cli.StringFlag{Name: "repo", Usage: "repo root override"},
			&cli.BoolFlag{Name: "skip-hooks"},
			&cli.BoolFlag{Name: "skip-prepare"},
			&cli.BoolFlag{Name: "foreground", Usage: "force fg execution of postcreate + prepare"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt create <branch>")
			}
			branch := c.Args().First()

			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			cfg, err := config.LoadLayered(repoRoot)
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
				return fmt.Errorf("destination path already exists: %s", wtPath)
			}
			if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
				return err
			}

			// Git worktree add. Default base = origin/HEAD if -b
			// missing.
			base := c.String("from")
			if base == "" {
				base = detectDefaultBranch(repoRoot)
			}
			branchExists := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
			var gitArgs []string
			if branchExists {
				gitArgs = []string{"-C", repoRoot, "worktree", "add", wtPath, branch}
			} else {
				gitArgs = []string{"-C", repoRoot, "worktree", "add", "-b", branch, wtPath, base}
			}
			cmd := exec.CommandContext(ctx, "git", gitArgs...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("git worktree add: %w", err)
			}
			wtPath = MustAbs(wtPath)

			// worktrees.links — symlinks from main into the new
			// worktree.
			for _, rel := range cfg.Worktrees.Links {
				src := filepath.Join(repoRoot, rel)
				dst := filepath.Join(wtPath, rel)
				if _, err := os.Stat(src); err != nil {
					PrintWarn("link source missing, skipping: %s", src)
					continue
				}
				if _, err := os.Stat(dst); err == nil {
					continue
				}
				_ = os.MkdirAll(filepath.Dir(dst), 0o755)
				if err := os.Symlink(src, dst); err != nil {
					return fmt.Errorf("symlink %s → %s: %w", dst, src, err)
				}
			}

			sl := slug.For(wtPath, branch)
			tplCtx := template.FromSlug(sl)

			// env_scoping patches.
			pairs := make([]patcher.Pair, 0, len(cfg.EnvScoping.Patches))
			for _, p := range cfg.EnvScoping.Patches {
				v, err := template.Render(p.Template, tplCtx)
				if err != nil {
					return fmt.Errorf("render env_scoping patch %s: %w", p.Key, err)
				}
				pairs = append(pairs, patcher.Pair{Key: p.Key, Value: v})
			}
			for _, f := range cfg.EnvScoping.Files {
				abs := filepath.Join(wtPath, f)
				var outcome patcher.Outcome
				if filepath.Ext(abs) == ".xml" {
					outcome, err = patcher.PatchPhpunitFile(abs, pairs)
				} else {
					outcome, err = patcher.PatchEnvFile(abs, pairs)
				}
				if err != nil {
					return err
				}
				if outcome == patcher.Updated {
					fmt.Printf("patched %s\n", abs)
					if cfg.EnvScoping.SkipWorktree == nil || *cfg.EnvScoping.SkipWorktree {
						_, _ = patcher.SkipWorktree(repoRoot, abs)
					}
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
				return nil
			}

			env := CaptureInheritedEnv()

			// precreate is the sync phase.
			if len(cfg.Hooks.Precreate) > 0 {
				out, err := hooks.RunPrecreateHooks(ctx, cfg.Hooks.Precreate, repoRoot, wtPath, sl.Value, env)
				if err != nil {
					return err
				}
				fmt.Printf("precreate: exit=%d\n", out.AggregateExitCode)
				if out.AggregateExitCode != 0 {
					return fmt.Errorf("precreate failed")
				}
			}

			// Async via daemon when async_create is true (default).
			asyncCreate := cfg.Worktrees.AsyncCreate == nil || *cfg.Worktrees.AsyncCreate
			needsWork := len(cfg.Hooks.Postcreate) > 0 || (!c.Bool("skip-prepare") && len(cfg.Databases) > 0)
			if asyncCreate && !c.Bool("foreground") && !c.Bool("skip-prepare") && needsWork {
				resp, err := rpc.Call(ctx, rpc.Request{
					Method: rpc.MethodWorktreeFinalize,
					WorktreeFinalize: &rpc.WorktreeFinalizeArgs{
						RepoPath:     repoRoot,
						WorktreePath: wtPath,
						InheritedEnv: env,
					},
				})
				if err != nil {
					PrintWarn("daemon RPC failed (%v); falling back to foreground", err)
				} else if resp.Kind == rpc.KindWorktreeFinalizeQueued {
					PrintOK("queued: postcreate + prepare detached to daemon — follow with `treeman logs tail -f`")
					return nil
				} else if resp.Kind == rpc.KindError {
					PrintWarn("daemon: %s; falling back to foreground", resp.Message)
				}
			}

			// Foreground tail.
			if len(cfg.Hooks.Postcreate) > 0 {
				if _, err := hooks.RunHooks(ctx, "postcreate", cfg.Hooks.Postcreate, repoRoot, wtPath, sl.Value, env); err != nil {
					return err
				}
				fmt.Printf("postcreate: %d group(s) spawned (logs in %s/.treeman-hooks/)\n",
					len(cfg.Hooks.Postcreate), wtPath)
			}
			if !c.Bool("skip-prepare") && len(cfg.Databases) > 0 {
				outs, err := prepare.Run(ctx, &cfg, repoRoot, wtPath, sl, st, repoID, wtID, env)
				if err != nil {
					PrintWarn("prepare failed: %v", err)
				}
				for _, o := range outs {
					fmt.Printf("prepare[%s] %s template=%s clones=%d\n",
						o.Engine, o.SourceDB, o.TemplateName, len(o.Clones))
				}
			}
			return nil
		},
	}
}

func wtDelete() *cli.Command {
	return &cli.Command{
		Name:    "delete",
		Aliases: []string{"rm"},
		Usage:   "delete a worktree end-to-end",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo"},
			&cli.BoolFlag{Name: "force", Aliases: []string{"f"}},
			&cli.BoolFlag{Name: "foreground", Usage: "force fg execution of predelete + teardown + git remove"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt delete <path-or-branch>")
			}
			target := c.Args().First()
			wtPath := MustAbs(target)

			repoRoot, err := resolveRepo(c.String("repo"))
			if err == nil {
				_ = repoRoot
			} else {
				repoRoot, err = DiscoverRepoRoot(wtPath)
				if err != nil {
					return err
				}
			}
			cfg, err := config.LoadLayered(repoRoot)
			if err != nil {
				return err
			}
			env := CaptureInheritedEnv()

			asyncDelete := cfg.Worktrees.AsyncDelete == nil || *cfg.Worktrees.AsyncDelete
			if asyncDelete && !c.Bool("foreground") {
				resp, err := rpc.Call(ctx, rpc.Request{
					Method: rpc.MethodWorktreeTeardown,
					WorktreeTeardown: &rpc.WorktreeTeardownArgs{
						RepoPath:     repoRoot,
						WorktreePath: wtPath,
						Force:        c.Bool("force"),
						InheritedEnv: env,
					},
				})
				if err != nil {
					PrintWarn("daemon RPC failed (%v); falling back to foreground", err)
				} else if resp.Kind == rpc.KindWorktreeTeardownQueued {
					PrintOK("queued: predelete + DB teardown + git remove detached to daemon — follow with `treeman logs tail -f`")
					return nil
				} else if resp.Kind == rpc.KindError {
					PrintWarn("daemon: %s; falling back to foreground", resp.Message)
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
				_, _ = hooks.RunHooks(ctx, "predelete", cfg.Hooks.Predelete, repoRoot, wtPath, sl.Value, env)
			}
			_ = prepare.TeardownDatabases(ctx, &cfg, sl.Value, repoID, wtID, st)
			args := []string{"-C", repoRoot, "worktree", "remove"}
			if c.Bool("force") {
				args = append(args, "--force")
			}
			args = append(args, wtPath)
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil && !c.Bool("force") {
				return fmt.Errorf("git worktree remove: %w", err)
			}
			_ = st.MarkWorktreeDeleted(ctx, wtID)
			PrintOK("deleted worktree #%d (%s)", wtID, wtPath)
			return nil
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
		Action: func(ctx context.Context, c *cli.Command) error {
			dbPath, _ := store.DefaultDBPath()
			st, err := store.Open(ctx, dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			rows, err := st.DB.QueryContext(ctx, `SELECT id, slug, COALESCE(branch,'-'), path
				FROM worktrees WHERE deleted_at IS NULL ORDER BY id`)
			if err != nil {
				return err
			}
			defer rows.Close()
			fmt.Printf("%-4s %-24s %-30s %s\n", "ID", "SLUG", "BRANCH", "PATH")
			for rows.Next() {
				var id int64
				var slug, branch, path string
				if err := rows.Scan(&id, &slug, &branch, &path); err != nil {
					return err
				}
				fmt.Printf("%-4d %-24s %-30s %s\n", id, slug, branch, path)
			}
			return rows.Err()
		},
	}
}

func wtFinalize() *cli.Command {
	return &cli.Command{
		Name:  "finalize",
		Usage: "ask the daemon to rerun postcreate + prepare for a worktree",
		Flags: []cli.Flag{&cli.StringFlag{Name: "repo"}},
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
			PrintOK("queued: postcreate + prepare detached to daemon — follow with `treeman logs tail -f`")
			return nil
		},
	}
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
		raw = "../worktrees"
	}
	if filepath.IsAbs(raw) {
		return raw
	}
	return filepath.Join(repoRoot, raw)
}

func detectDefaultBranch(repoRoot string) string {
	out, err := exec.Command("git", "-C", repoRoot, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		s := string(out)
		s = trimSpace(s)
		if len(s) > len("origin/") && s[:len("origin/")] == "origin/" {
			return s[len("origin/"):]
		}
		return s
	}
	out, err = exec.Command("git", "-C", repoRoot, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err == nil {
		return trimSpace(string(out))
	}
	return "main"
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
