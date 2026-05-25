# MCP / AI integration

[← back to README](../README.md)

treeman ships an MCP (Model Context Protocol) server so AI agents
— Claude Code, Claude Desktop, Cursor, Continue, anything that
speaks MCP — can drive the same lifecycle a human drives from the
CLI. Transport is stdio; no extra processes, no network surface.

```sh
treeman mcp                                    # all tools exposed
```

`treeman mcp` takes no permission flags — every tool (read, write,
engine introspection, worktree lifecycle) is exposed unconditionally.
The MCP surface is designed as the fully-qualified link to treeman;
clients that want a restricted surface should enforce that at the
agent-policy layer (Claude Code's tool allow-list, Cursor's MCP
allow rules, etc.), not here.

## Tools exposed

| Tool | What it does |
|---|---|
| `doctor`, `daemon_status` | Health checks. |
| `config_get`, `config_validate`, `config_schema` | Read/validate the YAML config. `config_get` output is redacted (passwords in resolved connection strings). |
| `worktree_list`, `worktree_show`, `snapshots_list` | Registry + snapshot-cache queries. |
| `logs_query`, `logs_hooks` | Event log + hook run history. Output is run through a secret-redaction pass (URI userinfo, AWS/GitHub tokens, JWTs, `KEY=value` for password/secret/token-shaped keys) before returning to the client. |
| `fw_detect`, `slug_compute` | Detection helpers. |
| `config_write`, `config_set`, `hook_run`, `prepare_run` | Replace the whole YAML body, patch a single field by dotted path, run a hook phase, run the prepare pipeline. |
| `init_repo`, `schema_install` | Scaffold `.treeman.yaml`; install the JSON Schema (`target=repo` / `target=global` / `target=url`) and wire its modeline. Both in-process. |
| `registry_register`, `registry_unregister`, `registry_repair` | Mutate the SQLite worktree registry directly. `repair` diffs `git worktree list` vs SQLite and auto-reconciles drift. |
| `snapshots_purge`, `logs_purge` | Wipe the snapshot cache (forces next prepare to rebuild) / delete event-log rows by filter (at least one filter required). |
| `daemon_control` | Start / stop treemand. Prefers the installed systemd/launchd unit; otherwise forks the `treemand` binary (start) or sends the shutdown RPC (stop). |
| `worktree_create`, `worktree_delete` | Run the full git + hooks + prepare lifecycle in-process via `internal/wt`. The heavy tail (hooks + prepare for create; teardown for delete) is dispatched to the daemon; on daemon-unreachable the orchestrator spawns a detached `treeman wt finalize --local` / `wt delete --detached` child and returns immediately. Returns a structured result (`wt_path`, `status`, `slug`, `worktree_id`, `log_path`). |

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
