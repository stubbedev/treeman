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
package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/version"
)

// Options controls the surface area exposed to the MCP client.
//
// AllowMutations gates every tool that writes to disk, mutates the
// SQLite registry, or executes shell commands (hooks, prepare, wt
// create/delete, config patch, init). Read tools and resources are
// always available. Default false — clients must opt in.
//
// AllowShellOps additionally gates tools that shell out to the
// `treeman` binary (worktree_create, worktree_delete). These can
// take many seconds and execute arbitrary user-defined hook
// commands, so they're behind a second flag in case an operator
// wants quick config patches without giving the agent full shell
// access. Implied by AllowMutations unless explicitly false.
type Options struct {
	AllowMutations bool
	AllowShellOps  bool
}

// Serve boots the MCP server on stdio and blocks until the client
// disconnects or ctx is cancelled. Returns nil on clean shutdown.
func Serve(ctx context.Context, opts Options) error {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "treeman",
		Version: version.Version,
	}, nil)

	registerReadTools(srv)
	registerResources(srv)
	if opts.AllowMutations {
		registerWriteTools(srv, opts)
	}

	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}
