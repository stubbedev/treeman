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

// newServer builds the fully-registered MCP server without starting a
// transport. Extracted from Serve so tests can introspect the tool +
// resource surface over an in-memory transport.
func newServer() *mcpsdk.Server {
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

	registerReadTools(srv)
	registerResources(srv)
	registerWriteTools(srv)
	registerPrompts(srv)
	return srv
}

// serverInstructions is the one-shot briefing every MCP client gets
// during initialize. Lists the trigger phrases that should pull treeman
// into the conversation and the highest-leverage entry points, so an
// agent doesn't fall back to raw `git`/`psql` when treeman owns the
// flow. Keep it short — clients prepend it to the model's context.
const serverInstructions = `Treeman manages per-git-worktree database snapshots: every branch gets its own ephemeral MySQL/Postgres/Mongo/Redis/ES instance, cold-built from a dump + migrations and cached as a template for fast reuse.

REACH FOR TREEMAN when the user mentions: worktree, branch-scoped DB, snapshot/template, prepare, dump+migrate+seed, ` + "`.treeman.yaml`" + `, treemand, finalize, fingerprint, fanout/clone target, slug.

PREFERRED ENTRY POINTS:
- "what's wrong / why did this fail" → doctor first, then logs_query (or the diagnose-prepare-failure prompt for the full chain).
- "set up treeman in this repo from scratch" → the bootstrap-new-repo prompt.
- "set me up a worktree for branch X" → the worktree-setup prompt (drives branches_list → daemon_status → worktree_create → wait).
- "what's the daemon doing right now" → daemon_state (in-flight prepares + watchers + backoff). For just version/PID use daemon_status.
- "is my DB up / are engines reachable" → engine_status. For a specific connection string before committing it, connection_probe.
- "edit .treeman.yaml" → config_get → config_diff → config_set (surgical) or config_write (full body).
- "why did this prepare cold-build instead of cache-hit" → inputs_fingerprint, then snapshot_inspect on the expected fingerprint.
- "what would prepare actually run" → prepare_dry_run (renders the plan; no engine I/O).
- "stream prepare progress" → logs_subscribe with a progressToken (live notifications) — falls back to collect-on-return.
- "this worktree is stuck / broken" → worktree_repair (reconciles ports + finalize + snapshot templates).
- "trial this migration without polluting state" → the migration-trial prompt.
- "scaffold .treeman.yaml" → the scaffold-from-framework prompt.

DESTRUCTIVE TOOLS support dry_run=true for previewing: worktree_delete, snapshots_purge, db_reset, repo_remove. Always preview before committing.

DO NOT shell out to ` + "`git worktree`, `psql`, `mysql`, `mongosh`, `redis-cli`" + ` for repo state already covered by these tools — treeman keeps SQLite + engine state in sync, and bypassing it leaves the two out of sync.

Destructive tools (worktree_delete, snapshots_purge, snapshot_drop, db_reset, registry_unregister, repo_remove, logs_purge) carry DestructiveHint=true in their annotations — surface the consequence to the user before invoking.`

// Serve boots the MCP server on stdio and blocks until the client
// disconnects or ctx is cancelled. Returns nil on clean shutdown.
func Serve(ctx context.Context) error {
	if err := newServer().Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}
