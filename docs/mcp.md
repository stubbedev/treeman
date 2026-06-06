# MCP / AI integration

[← back to README](../README.md)

treeman ships an MCP (Model Context Protocol) server so AI agents
— Claude Code, Claude Desktop, Cursor, Continue, anything that
speaks MCP — get a structured tool surface onto treeman's state.
Transport is stdio; no extra processes, no network surface.

```sh
treeman mcp                                    # all tools exposed
```

## What is this actually for

Four use cases drive the MCP surface, each backed by tools an
agent would otherwise have to drive by reading source + scrolling
logs:

- **Configuration assistance.** An agent can read the current
  `.treeman.yaml` (`config_get`), validate edits against the
  authoritative schema (`config_validate`, `config_schema`), patch
  individual fields by dotted path (`config_set`), or scaffold a
  fresh config for a repo it just detected
  (`init_repo`, `fw_detect`, `schema_install`). This is the
  biggest win — `.treeman.yaml` is the surface that decides
  whether a worktree boots, and editing it without treeman's own
  validator is where humans burn the most time.

- **Diagnosis when prepare fails.** `logs_query` (event log) +
  `logs_hooks` (hook run summaries) + `hook_log_read` (full
  per-hook stdout/stderr) give the agent everything the daemon
  recorded. `worktree_show` is the per-worktree dossier.
  `doctor` runs the health-check matrix. `snapshots_inspect`
  resolves a fingerprint to "is this row an orphan or live?" —
  the canonical answer for "cache hit but prepare still failed".

- **Live engine state.** `db_query` runs READ-ONLY queries
  (SELECT/SHOW/EXPLAIN on SQL engines; find-style filters on
  Mongo; `_search` bodies on ES; one of {GET, SCAN, KEYS, INFO,
  …} on Redis). `db_schema_dump` returns the live CREATE TABLEs.
  `engine_status` probes every engine declared in
  `.treeman.yaml`. Use these to verify a migration's effect or
  reason about live shape vs. what the config says.

- **Snapshot-cache maintenance.** `snapshots_list` shows what's
  cached for the current repo; `snapshots_drop` evicts a single
  fingerprint when a template went stale; `snapshots_purge`
  wipes the whole cache.

## Tool discovery (lazy disclosure)

To keep the model's context lean, `treeman mcp` advertises only a
curated **core** set of tools up front (`doctor`, `worktree_*`,
`prepare_run`, `logs_query`, `config_get`/`config_set`,
`daemon_status`/`daemon_state`, `status_overview`, `branches_list`,
`sync_status`, `prompts_list`) plus one **`tools`** gateway. The rest
are deferred:

- `tools` with `action=list` returns every available tool grouped by
  category with a one-line summary (no schemas).
- `tools` with `action=enable` + `names=[…]` (or `category=…`) loads
  the chosen tools' full schemas so they become callable; the client is
  notified via `tools/list_changed`.

Set `TREEMAN_MCP_ALL_TOOLS=1` to advertise every tool at startup
instead (the pre-disclosure behavior). The full surface is always
*reachable* — lazy mode only controls what's loaded into context first.

## Permission model

`treeman mcp` takes no permission flags — every tool (read, write,
engine introspection, worktree lifecycle) is reachable (loaded up front
or via the `tools` gateway above). The MCP surface is designed as the
fully-qualified link to treeman; clients that want a restricted surface
should enforce that at the agent-policy layer (Claude Code's tool
allow-list, Cursor's MCP allow rules, etc.), not here.

### Destructive-action confirmation (elicitation)

`worktree_delete`, `snapshots_purge`, `db_reset`, and `repo_remove`
gate their mutation behind an MCP `notifications/elicitation`
confirmation when invoked with `dry_run=false`. Clients that support
elicitation (Claude Desktop, etc.) get a confirmation pop-up before
the action runs; clients that don't support it (or that error out)
fall through to proceed so non-interactive agents aren't blocked.

Per-call overrides:

- `dry_run=true` — skip elicitation; return the plan instead.
- `ack=true` — skip elicitation; proceed (use when the agent has
  already secured user approval out-of-band).

Refusals (decline/cancel) come back as `refused: "<reason>"` on the
tool result with no mutation performed.

## Tools exposed

The complete tool list — grouped by category, flagged **core** (loaded
up front) vs revealed on demand — is generated from the live registry:
**[mcp-tools.md](mcp-tools.md)**. It refreshes via `just sync-docs`, so it
never drifts from the code (CI fails a PR that forgets to regenerate).

## Resources

Resources are read-only context attachments — cheaper than re-invoking
tools each turn.

| URI | What |
|---|---|
| `treeman://config/raw` | The repo's `.treeman.yaml` byte-for-byte. |
| `treeman://config/resolved` | Post-substitution view (env vars + connection strings rendered, credentials redacted). |
| `treeman://config/schema` | JSON Schema for `.treeman.yaml`. |
| `treeman://logs/recent` | The 200 most recent event-log rows (NDJSON). |
| `treeman://worktrees/{slug}/events` | Per-worktree event-log slice. |
| `treeman://worktrees/{slug}/hooks` | Per-worktree hook_run rows. |
| `treeman://daemon/state` | Mirrors `daemon_state` — in-flight prepares/teardowns, watcher set, backoff timers. |
| `treeman://repos/{repo}/snapshots` | Cached snapshots for one repo. Use `cwd` for the placeholder to mean "the current dir's repo". |
| `treeman://repos/{repo}/branches` | Branch list with worktree occupancy. Same `cwd` placeholder. |

## Prompts

Prompts encode multi-step workflows so an agent doesn't reinvent each
flow. Invoke from your MCP client to get a tailored briefing.

| Name | What |
|---|---|
| `bootstrap-new-repo` | First-time enrollment: detect → probe engines → init → schema_install → daemon ensure → register → first prepare. |
| `scaffold-from-framework` | Detect → scaffold `.treeman.yaml` → validate → review. Stops short of running prepare. |
| `worktree-setup` | Pick branch → daemon ensure → create → wait → verify. |
| `diagnose-prepare-failure` | Drives `logs_query` → `engine_status` → `snapshots_inspect` → root-cause report. |
| `cache-cleanup` | Hunt orphan snapshots (template gone, SQLite row remains) and drop only those. |
| `migration-trial` | Throw-away worktree → run migration → schema diff → tear down. |

## Claude Code

[Claude Code](https://claude.ai/code) ships a `claude mcp add`
command:

```sh
# User-scoped (available across every project on this machine)
claude mcp add --scope user treeman -- treeman mcp

# Project-scoped (committed to .mcp.json in the repo root)
claude mcp add --scope project treeman -- treeman mcp

# Local-scoped (private to you in this project — `claude mcp add`'s default)
claude mcp add treeman -- treeman mcp
```

Confirm it registered with `claude mcp list`. Inside a session,
the tools appear as `mcp__treeman__<tool-name>` (e.g.
`mcp__treeman__worktree_list`).

## Claude Desktop

Edit the desktop config — on macOS:
`~/Library/Application Support/Claude/claude_desktop_config.json`;
on Linux: `~/.config/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "treeman": {
      "command": "treeman",
      "args": ["mcp"]
    }
  }
}
```

Restart Claude Desktop. The tools surface under the hammer icon.

## Cursor

Add to `~/.cursor/mcp.json` (global) or `<repo>/.cursor/mcp.json`
(per-project):

```json
{
  "mcpServers": {
    "treeman": {
      "command": "treeman",
      "args": ["mcp"]
    }
  }
}
```

## Generic MCP client

Any client that supports stdio MCP servers takes the same shape:
the command is `treeman`, the args are `["mcp", ...flags]`.
Stderr is logging; stdout is the JSON-RPC envelope, so do not
wrap the command in anything that mixes the two.

## Security notes

- The MCP server runs **as the invoking user** with full access
  to the repo, the daemon socket, and any `.env` files. Lazy
  disclosure (see *Tool discovery*) controls what loads into context
  first, but every tool stays reachable — it is not a security gate.
  Restrict the exposed surface at the **agent-policy layer**
  (Claude Code's per-tool allow rules, Cursor's MCP allow list,
  etc.) for any agent you don't fully trust.
- `worktree_delete` from MCP runs `wt.Delete` in-process. The
  TTY confirmation prompt is a CLI-only concern (it lives in
  the `wt delete` action closure, not the orchestrator), so MCP
  callers get **no confirmation** — every `worktree_delete`
  invocation runs the teardown.
- Hook stdout/stderr and event payloads pass through
  `redactSecrets` (see `internal/mcp/ops.go`) before being
  returned to the client. False positives just hide a token;
  false negatives leak it. Project-specific patterns can be
  added to `secretPatterns`.
