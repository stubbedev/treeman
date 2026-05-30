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
  `doctor` runs the health-check matrix. `snapshot_inspect`
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
  cached for the current repo; `snapshot_drop` evicts a single
  fingerprint when a template went stale; `snapshots_purge`
  wipes the whole cache.

## Permission model

`treeman mcp` takes no permission flags — every tool (read, write,
engine introspection, worktree lifecycle) is exposed unconditionally.
The MCP surface is designed as the fully-qualified link to treeman;
clients that want a restricted surface should enforce that at the
agent-policy layer (Claude Code's tool allow-list, Cursor's MCP
allow rules, etc.), not here.

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

| Tool | What it does |
|---|---|
| `doctor`, `daemon_status`, `daemon_state` | Health checks. `daemon_state` adds a rich runtime view: in-flight finalize/teardown work, watcher set, per-repo sync backoff timers. |
| `config_get`, `config_validate`, `config_schema`, `config_diff` | Read/validate the YAML config. `config_get` output is redacted (passwords in resolved connection strings). |
| `worktree_list`, `worktree_show`, `snapshots_list` | Registry + snapshot-cache queries. `worktree_show` also reports allocated ports and branch_scoped active-namespace state. |
| `branch_scoped_status` | Per branch_scoped database: active namespace, which branch's data occupies it now, and which local branches have a resumable durable copy. |
| `logs_query`, `logs_hooks`, `logs_wait`, `logs_subscribe` | Event log + hook run history. `logs_wait` blocks until N matches via SQLite polling; **`logs_subscribe` prefers push mode** — opens a streaming RPC subscription to the daemon so events arrive without polling, falling back to 500ms polling only when the daemon is unreachable. The result's `mode` field reports which path was used. Output runs through a secret-redaction pass. |
| `fw_detect`, `slug_compute`, `inputs_fingerprint` | Detection + fingerprint helpers. `inputs_fingerprint` answers "why did prepare cold-build instead of cache-hit?". |
| `prepare_dry_run` | Render the prepare pipeline plan WITHOUT executing — per-DB rendered name, dump files, migrate/seed commands, fanout count, expected fingerprint. |
| `connection_probe` | Dry-test a connection string (or the repo's configured connection) — reachable, version, latency. Use to iterate on credentials before committing them. |
| `engine_logs` | Tail container logs for one configured engine (`docker logs --tail N --since S` or `podman`/`nerdctl`/`finch` per the connection block). Closes the "why is MySQL refusing connections?" gap. Errors with a clear message when the engine has no container ref. |
| `prompts_list` | List every registered MCP prompt with its when-to-use trigger. Discovery backup for clients with weak prompt UI. |
| `config_write`, `config_set`, `hook_run`, `prepare_run` | Replace the whole YAML body, patch a single field by dotted path, run a hook phase (`env_overrides` lets you tweak one var for the run), run the prepare pipeline. |
| `db_reset` | Re-sync a worktree's `branch_scoped` databases from the live base branch. Destructive for the current branch's working data; `dry_run=true` previews, `ack=true` skips elicitation. |
| `init_repo`, `schema_install` | Scaffold `.treeman.yaml`; install the JSON Schema and wire its modeline. |
| `registry_register`, `registry_unregister`, `registry_repair`, `worktree_repair` | Mutate the SQLite worktree registry directly. `registry_repair` diffs git vs SQLite. `worktree_repair` reconciles ports / finalize state / snapshot templates for one worktree. |
| `repo_remove` | Drop a whole REPO from the SQLite registry (cascades to its worktrees/events/snapshots/hook_runs). `dry_run=true` counts cascaded rows first. External resources are not touched; refuses by default if active worktrees exist. |
| `snapshots_purge`, `logs_purge` | Wipe the snapshot cache (`dry_run=true` previews) / delete event-log rows by filter (at least one filter required). |
| `daemon_control` | Start / stop treemand. Prefers the installed systemd/launchd unit; otherwise forks the `treemand` binary (start) or sends the shutdown RPC (stop). |
| `worktree_create`, `worktree_delete` | Run the full git + hooks + prepare lifecycle in-process via `internal/wt`. `worktree_delete` accepts `dry_run=true` to preview the per-engine namespaces that would be dropped. The heavy tail is dispatched to the daemon; on daemon-unreachable the orchestrator spawns a detached child and returns immediately. |

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
| `diagnose-prepare-failure` | Drives `logs_query` → `engine_status` → `snapshot_inspect` → root-cause report. |
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
  to the repo, the daemon socket, and any `.env` files. Every
  tool is exposed unconditionally — there is no in-binary gate.
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
