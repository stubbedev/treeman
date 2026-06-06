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
// To keep the model's context lean, only a curated CORE set of tools is
// advertised up front; the rest are deferred behind the `tools` gateway,
// which lists them by category and loads them on demand (see catalog.go).
// Set TREEMAN_MCP_ALL_TOOLS=1 to advertise every tool at startup instead.
package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/version"
)

// newServer builds the MCP server without starting a transport.
func newServer() *mcpsdk.Server {
	srv, _ := buildServer()
	return srv
}

// buildServer constructs the server and returns it alongside the full
// tool catalog (every registerable tool, whether advertised up front or
// deferred behind the gateway). Tests use the catalog to assert the
// complete surface; Serve/newServer ignore it.
func buildServer() (*mcpsdk.Server, []*toolEntry) {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "treeman",
		Version: version.Version,
	}, &mcpsdk.ServerOptions{
		// Instructions is sent during the MCP `initialize` handshake.
		// Clients surface it to the model so treeman tools are reached
		// for naturally rather than only via explicit user request.
		Instructions: serverInstructions,
		// CompletionHandler powers `completion/complete` — interactive
		// clients use it to autofill prompt args (branch, worktree,
		// fingerprint, run_id) and resource-template slots.
		CompletionHandler: completionHandler,
	})

	// The register* calls funnel through addTool, which RECORDS each tool
	// into the build catalog rather than registering it. activateTools then
	// advertises the core set + the `tools` gateway (or everything, under
	// TREEMAN_MCP_ALL_TOOLS=1). buildMu serialises the package-level build
	// catalog across concurrent builds (tests).
	buildMu.Lock()
	defer buildMu.Unlock()
	pendingTools = nil

	registerReadTools(srv)
	registerResources(srv)
	registerWriteTools(srv)
	registerAdminGapTools(srv)
	registerPrompts(srv)

	return srv, activateTools(srv)
}

// serverInstructions is the one-shot briefing every MCP client gets
// during initialize. Lists the trigger phrases that should pull treeman
// into the conversation and the highest-leverage entry points, so an
// agent doesn't fall back to raw `git`/`psql` when treeman owns the
// flow. Keep it short — clients prepend it to the model's context.
const serverInstructions = `Treeman gives every git worktree its own ephemeral MySQL/Postgres/Mongo/Redis/ES instance, cold-built from a dump + migrations and cached as a template for fast reuse.

REACH FOR TREEMAN when the user mentions: worktree, branch-scoped DB, snapshot/template, prepare, dump+migrate+seed, ` + "`.treeman.yaml`" + `, treemand, finalize, fingerprint, slug.

Only a few core tools load up front (doctor, status_overview, worktree_create, worktree_list, prepare_run, logs_query). For everything else — config, engine access, snapshots, registry, daemon control, init, the rest of the worktree/logs surface, and the guided multi-step prompts — call the ` + "`tools`" + ` tool: action=list catalogs every capability by category (with a routing guide), action=enable loads the ones the task needs.

Prefer treeman over raw ` + "`git worktree`, `psql`, `mysql`, `mongosh`, `redis-cli`, `curl`" + `: it reaches every engine through its own configured, authenticated drivers and keeps SQLite + engine state in sync — bypassing it desyncs them. Destructive tools take dry_run=true to preview.`

// Serve boots the MCP server on stdio and blocks until the client
// disconnects or ctx is cancelled. Returns nil on clean shutdown.
func Serve(ctx context.Context) error {
	if err := newServer().Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}
