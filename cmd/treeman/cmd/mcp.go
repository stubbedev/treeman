package cmd

import (
	"context"

	"github.com/urfave/cli/v3"

	mcpsrv "github.com/stubbedev/treeman/internal/mcp"
)

// MCPCmd — `treeman mcp` runs a Model Context Protocol server so AI
// agents can drive treeman the same way a human does from the CLI.
// Every tool (read, write, engine introspection, shell-spawning) is
// exposed unconditionally — the MCP surface is designed as the
// fully-qualified link to treeman. Clients that want a restricted
// surface should enforce that at the agent-policy layer, not here.
//
// The default transport is stdio (one server per client). Passing
// --http (or setting TREEMAN_MCP_HTTP_ADDR) switches to the streamable
// HTTP transport, where a single shared server backs every client —
// each request pinned to its own worktree via the X-Repo-Root header or
// the client's MCP roots. See internal/mcp/http.go.
//
// Typical client wiring (Claude Desktop / agent runtimes):
//
//	{
//	  "mcpServers": {
//	    "treeman": { "command": "treeman", "args": ["mcp"] }
//	  }
//	}
//
// Shared HTTP daemon:
//
//	treeman mcp --http                       # loopback :8787, path /mcp
//	treeman mcp --http=127.0.0.1:9000        # custom bind
//
//	{
//	  "mcpServers": {
//	    "treeman": {
//	      "type": "http",
//	      "url": "http://127.0.0.1:8787/mcp",
//	      "headers": { "X-Repo-Root": "/abs/path/to/worktree" }
//	    }
//	  }
//	}
func MCPCmd() *cli.Command {
	return &cli.Command{
		Name:  "mcp",
		Usage: "run the Model Context Protocol server (stdio, or --http for a shared HTTP daemon)",
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
			args := c.Args().Slice()
			for _, a := range args {
				if a == "-h" || a == "--help" {
					return cli.ShowSubcommandHelp(c)
				}
			}
			if addr, path := mcpsrv.HTTPConfig(args); addr != "" {
				return mcpsrv.ServeHTTP(ctx, addr, path)
			}
			return mcpsrv.Serve(ctx)
		},
	}
}
