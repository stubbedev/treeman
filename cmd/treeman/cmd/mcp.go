package cmd

import (
	"context"

	"github.com/urfave/cli/v3"

	mcpsrv "github.com/stubbedev/treeman/internal/mcp"
)

// MCPCmd — `treeman mcp` runs a Model Context Protocol server over
// stdio so AI agents can drive treeman the same way a human does
// from the CLI. Every tool (read, write, engine introspection,
// shell-spawning) is exposed unconditionally — the MCP surface is
// designed as the fully-qualified link to treeman. Clients that
// want a restricted surface should enforce that at the agent-policy
// layer, not here.
//
// Typical client wiring (Claude Desktop / agent runtimes):
//
//	{
//	  "mcpServers": {
//	    "treeman": {
//	      "command": "treeman",
//	      "args": ["mcp"]
//	    }
//	  }
//	}
func MCPCmd() *cli.Command {
	return &cli.Command{
		Name:  "mcp",
		Usage: "run the Model Context Protocol server (stdio transport)",
		// MCP clients (Claude Desktop, Claude Code, agent runtimes)
		// sometimes wire `treeman mcp` with extra flags they
		// invented for other servers — e.g. --allow-mutations,
		// --allow-shell. We expose every tool unconditionally
		// (see package comment), so those flags have no meaning
		// here. Skip flag parsing entirely instead of crashing
		// the server before stdio hands off — a hard error on
		// startup leaves the client wedged with "failed to
		// connect" and no useful diagnostic.
		SkipFlagParsing: true,
		Action: func(ctx context.Context, c *cli.Command) error {
			for _, a := range c.Args().Slice() {
				if a == "-h" || a == "--help" {
					return cli.ShowSubcommandHelp(c)
				}
			}
			return mcpsrv.Serve(ctx)
		},
	}
}
