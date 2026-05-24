// Package mcp serves treeman's tool + resource surface over the
// Model Context Protocol so AI agents can drive setup, debugging,
// and worktree orchestration the same way a human would from the
// CLI. The server is started by `treeman mcp` over stdio.
//
// The package is self-contained — it does not import cmd — so the
// MCP and CLI surfaces evolve independently. Shared orchestration
// helpers live in `ops.go` and call the same underlying internal
// packages (resolve, prepare, hooks, store, slug, rpc) that the
// CLI uses.
//
// Every tool — read, write, engine-introspection, shell-spawning —
// is registered unconditionally. The MCP server is designed as a
// fully-qualified link to all of treeman's observability,
// configuration, and execution capabilities. Clients that want to
// restrict the surface should do so at the agent-policy layer, not
// here.
package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/version"
)

// Serve boots the MCP server on stdio and blocks until the client
// disconnects or ctx is cancelled. Returns nil on clean shutdown.
func Serve(ctx context.Context) error {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "treeman",
		Version: version.Version,
	}, nil)

	registerReadTools(srv)
	registerResources(srv)
	registerWriteTools(srv)

	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}
