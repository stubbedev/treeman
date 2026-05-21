package cmd

import (
	"context"

	"github.com/urfave/cli/v3"

	mcpsrv "github.com/stubbedev/treeman/internal/mcp"
)

// MCPCmd — `treeman mcp` runs a Model Context Protocol server over
// stdio so AI agents can drive treeman the same way a human does
// from the CLI. Read-only tools + resources are always available;
// mutating tools (prepare, hooks, wt create/delete, config writes,
// init, schema install) only register when --allow-mutations is
// passed. Tools that shell out to the `treeman` binary are
// additionally gated by --allow-shell.
//
// Typical client wiring (Claude Desktop / agent runtimes):
//
//	{
//	  "mcpServers": {
//	    "treeman": {
//	      "command": "treeman",
//	      "args": ["mcp", "--allow-mutations", "--allow-shell"]
//	    }
//	  }
//	}
func MCPCmd() *cli.Command {
	return &cli.Command{
		Name:  "mcp",
		Usage: "run the Model Context Protocol server (stdio transport)",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "allow-mutations",
				Usage: "enable tools that modify config, run hooks/prepare, or invoke `treeman wt create|delete`",
			},
			&cli.BoolFlag{
				Name:  "allow-shell",
				Usage: "enable shell-out tools (worktree_create, worktree_delete, init_repo, schema_install, daemon_control). Implies --allow-mutations.",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			opts := mcpsrv.Options{
				AllowMutations: c.Bool("allow-mutations") || c.Bool("allow-shell"),
				AllowShellOps:  c.Bool("allow-shell"),
			}
			return mcpsrv.Serve(ctx, opts)
		},
	}
}
