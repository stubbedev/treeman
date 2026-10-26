// Package treemanapp constructs the urfave/cli v3 root command tree
// for the `treeman` binary. Lives in internal/ so both `cmd/treeman`
// (the binary itself) and `cmd/treeman-gen-docs` (the markdown
// generator that walks the same tree) can share one declaration.
package treemanapp

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/cmd/treeman/cmd"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/version"
)

// New returns the configured root *cli.Command. Every new top-level
// subcommand belongs in the Commands slice below; gen-docs walks
// from here so undocumented additions can't slip in.
func New() *cli.Command {
	root := newTree()
	rejectStrayArgs(root)
	return root
}

// rejectStrayArgs makes every leaf command that declares no positional
// arguments fail on one instead of running with it silently ignored:
// `treeman prepare status` ran a full prepare. A command that takes
// arguments declares them in ArgsUsage (or Arguments); a
// SkipFlagParsing command (mcp) reads its own argv and is left alone.
// The check runs in Before, after urfave has already answered shell
// completion and --help, so neither is affected.
func rejectStrayArgs(c *cli.Command) {
	for _, sub := range c.Commands {
		rejectStrayArgs(sub)
	}
	if len(c.Commands) > 0 || c.Action == nil || c.SkipFlagParsing || c.ArgsUsage != "" || len(c.Arguments) > 0 {
		return
	}
	before := c.Before
	c.Before = func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
		if cmd.NArg() > 0 {
			return ctx, fmt.Errorf("unexpected argument %q: `%s` takes no positional arguments",
				cmd.Args().First(), cmd.FullName())
		}
		if before != nil {
			return before(ctx, cmd)
		}
		return ctx, nil
	}
}

func newTree() *cli.Command {
	var restoreConfig func()
	return &cli.Command{
		Name:                  "treeman",
		Usage:                 "per-worktree DB orchestrator",
		Version:               version.Version,
		EnableShellCompletion: true,
		ConfigureShellCompletionCommand: func(c *cli.Command) {
			c.Hidden = false
		},
		Suggest: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Usage: "user-global config file (overrides TREEMAN_CONFIG)"},
			&cli.StringFlag{Name: "color", Value: "auto", Usage: "when to colorize output: auto | always | never (NO_COLOR beats always)"},
			&cli.BoolFlag{
				Name:  "require-daemon",
				Usage: "fail instead of falling back to in-process execution when treemand is unreachable (or set TREEMAN_REQUIRE_DAEMON=1)",
			},
		},
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			if err := ui.SetColorMode(c.String("color")); err != nil {
				return ctx, err
			}
			cmd.SetStrictDaemon(c.Bool("require-daemon") || os.Getenv("TREEMAN_REQUIRE_DAEMON") == "1")
			var err error
			restoreConfig, err = config.ConfigureGlobalPath(c.String("config"))
			return ctx, err
		},
		After: func(context.Context, *cli.Command) error {
			if restoreConfig != nil {
				restoreConfig()
				restoreConfig = nil
			}
			return nil
		},
		Commands: []*cli.Command{
			cmd.WorktreeCmd(),
			cmd.GitCmd(),
			cmd.ReposCmd(),
			cmd.StatusCmd(),
			cmd.MainCmd(),
			cmd.BranchesCmd(),
			cmd.PrepareCmd(),
			cmd.DbCmd(),
			cmd.SyncCmd(),
			cmd.HookCmd(),
			cmd.LogsCmd(),
			cmd.ConfigCmd(),
			cmd.SchemaCmd(),
			cmd.DaemonCmd(),
			cmd.FwCmd(),
			cmd.SlugCmd(),
			cmd.InitCmd(),
			cmd.DoctorCmd(),
			cmd.RegistryCmd(),
			cmd.SnapshotsCmd(),
			cmd.MCPCmd(),
			cmd.NotifyCmd(),
			cmd.PatchFilterCmd(),
			cmd.TuiCmd(),
		},
	}
}
