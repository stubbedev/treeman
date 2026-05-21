// Command treeman is the CLI for the treeman per-worktree DB
// orchestrator. The subcommand tree is treated as a stable user-
// facing contract so existing scripts (gwt zsh integration, CI
// flows) keep working across releases.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/cmd/treeman/cmd"
	"github.com/stubbedev/treeman/internal/version"
)

func main() {
	app := &cli.Command{
		Name:                  "treeman",
		Usage:                 "per-worktree DB orchestrator",
		Version:               version.Version,
		EnableShellCompletion: true,
		Commands: []*cli.Command{
			cmd.WorktreeCmd(),
			cmd.PrepareCmd(),
			cmd.HookCmd(),
			cmd.LogsCmd(),
			cmd.ConfigCmd(),
			cmd.SchemaCmd(),
			cmd.DaemonCmd(),
			cmd.FwCmd(),
			cmd.SlugCmd(),
			cmd.InitCmd(),
		},
	}
	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "treeman:", err)
		os.Exit(1)
	}
}
