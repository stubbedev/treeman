# MCP / AI integration

[← back to README](../README.md)

treeman ships an MCP (Model Context Protocol) server so AI agents
— Claude Code, Claude Desktop, Cursor, Continue, anything that
speaks MCP — can drive the same lifecycle a human drives from the
CLI. Transport is stdio; no extra processes, no network surface.

```sh
treeman mcp                                    # read-only tools
treeman mcp --allow-mutations                  # + write tools (config_write, hook_run, prepare_run)
treeman mcp --allow-mutations --allow-shell    # + shell-out tools (wt create/delete, init, schema install, daemon control)
```

Permissions stack — `--allow-shell` implies `--allow-mutations`.
The defaults are conservative on purpose: a fresh `treeman mcp`
call exposes only read-only tools, so an agent can inspect state
(list worktrees, query logs, run `doctor`, compute slugs) without
being able to change anything.

## Tools exposed

| Tool | Gate | What it does |
|---|---|---|
| `doctor`, `daemon_status` | read | Health checks. |
| `config_get`, `config_validate`, `config_schema` | read | Read/validate the YAML config. |
| `worktree_list`, `worktree_show` | read | Registry queries. |
| `logs_query`, `logs_hooks` | read | Event log + hook run history. Output is run through a secret-redaction pass (URI userinfo, AWS/GitHub tokens, JWTs, `KEY=value` for password/secret/token-shaped keys) before returning to the client. |
| `fw_detect`, `slug_compute` | read | Detection helpers. |
| `config_write`, `hook_run`, `prepare_run` | `--allow-mutations` | Write the YAML, run a hook phase, run the prepare pipeline. |
| `init_repo`, `schema_install`, `worktree_create`, `worktree_delete`, `daemon_control` | `--allow-shell` | Shell out to the `treeman` binary. |

## Claude Code

[Claude Code](https://claude.ai/code) ships a `claude mcp add`
command:

```sh
# User-scoped (available everywhere)
claude mcp add treeman -- treeman mcp --allow-mutations --allow-shell

# Project-scoped (committed to .mcp.json in the repo root)
claude mcp add --scope project treeman -- treeman mcp --allow-mutations
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
      "args": ["mcp", "--allow-mutations", "--allow-shell"]
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
      "args": ["mcp", "--allow-mutations"]
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
  to the repo, the daemon socket, and any `.env` files. Only
  enable `--allow-shell` for agents you trust to call
  `wt create/delete` on your behalf.
- `worktree_delete` from MCP runs through `treeman wt delete`,
  which prompts for confirmation on a TTY — but the MCP server's
  stdin is JSON-RPC, not a TTY, so the prompt is **skipped**.
  Treat `--allow-shell` accordingly.
- Hook stdout/stderr and event payloads pass through
  `redactSecrets` (see `internal/mcp/ops.go`) before being
  returned to the client. False positives just hide a token;
  false negatives leak it. Project-specific patterns can be
  added to `secretPatterns`.
