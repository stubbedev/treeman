# treeman docs

| Page | What you'll find |
|---|---|
| [cli.md](cli.md) | Full command reference, log filters, shell completion, output/color/paging, env vars |
| [configuration.md](configuration.md) | `.treeman.yaml` reference — every block, per-stack examples, templated names, hook groups, credential resolution, container DBs + hooks |
| [config-reference.md](config-reference.md) | Generated field-by-field `.treeman.yaml` reference (every key, type, and doc — auto-generated from the Go types) |
| [advanced.md](advanced.md) | Snapshot cache + GC, framework presets table |
| [mcp-tools.md](mcp-tools.md) | Generated MCP tool + prompt reference (every tool by category — from the registry) |
| [events.md](events.md) | Generated event-type reference (every `event_type` — from the `store.Evt*` constants) |
| [frameworks.md](frameworks.md) | Generated migration-framework preset table (markers + dirs — from the detector registry) |
| [rpc-reference.md](rpc-reference.md) | Generated RPC method / task / response-kind / param reference (from `internal/rpc`) |
| [mcp.md](mcp.md) | MCP / AI integration — Claude Code, Claude Desktop, Cursor, generic stdio clients, security notes |
| [internals.md](internals.md) | Storage layout, daemon model, init parity, RPC envelope, development workflow |

Start with [cli.md](cli.md) if you want to drive treeman from the
shell; [configuration.md](configuration.md) if you're writing the
YAML for a new repo; [mcp.md](mcp.md) if you're wiring it into an
AI agent.
