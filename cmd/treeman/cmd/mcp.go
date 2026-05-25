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
		Action: func(ctx context.Context, c *cli.Command) error {
			return mcpsrv.Serve(ctx)
		},
	}
}
