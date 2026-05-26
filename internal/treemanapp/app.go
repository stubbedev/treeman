// Package treemanapp constructs the urfave/cli v3 root command tree
// for the `treeman` binary. Lives in internal/ so both `cmd/treeman`
// (the binary itself) and `cmd/treeman-gen-docs` (the markdown
// generator that walks the same tree) can share one declaration.
package treemanapp

import (
	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/cmd/treeman/cmd"
	"github.com/stubbedev/treeman/internal/version"
)

// New returns the configured root *cli.Command. Every new top-level
// subcommand belongs in the Commands slice below; gen-docs walks
// from here so undocumented additions can't slip in.
func New() *cli.Command {
	return &cli.Command{
		Name:                  "treeman",
		Usage:                 "per-worktree DB orchestrator",
		Version:               version.Version,
		EnableShellCompletion: true,
		ConfigureShellCompletionCommand: func(c *cli.Command) {
			c.Hidden = false
		},
		Suggest: true,
		Commands: []*cli.Command{
			cmd.WorktreeCmd(),
			cmd.MainCmd(),
			cmd.BranchesCmd(),
			cmd.PrepareCmd(),
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
		},
	}
}
