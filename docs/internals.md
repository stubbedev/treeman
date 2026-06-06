# Internals — daemon, RPC, storage

[← back to README](../README.md)

## Storage layout

| Path | What |
|---|---|
| `~/.local/share/treeman/treeman.db` | SQLite event log + worktree registry + snapshots table (override with `$TREEMAN_DB_PATH`) |
| `~/.local/share/treeman/treemand.log` | Daemon stderr |
| `$XDG_RUNTIME_DIR/treeman.sock` | JSON-line RPC socket — overridable via `$TREEMAN_SOCKET`, falls back to `$XDG_DATA_HOME/treeman/treeman.sock` (SO_PEERCRED on Linux, stat-based owner check elsewhere) |
| `~/.config/systemd/user/treemand.service` | systemd-user unit (Linux) |
| `~/Library/LaunchAgents/dev.stubbe.treemand.plist` | launchd LaunchAgent (macOS) |
| `<worktree>/.treeman-hooks/<phase>-<n>.log` | Per-hook driver stdout/stderr |
| `<repo>/schemas/treeman.schema.json` | JSON Schema (only present after `treeman schema install`) |

The store schema lives in `internal/store/migrations/` (`0001_init.sql`
… and onward) and ships embedded into the binary, so a fresh
`treeman.db` self-migrates on first daemon start.

## Daemon model

`treemand` is the long-running process; `treeman` is a thin RPC
client that round-trips JSON over the unix socket. Why a daemon:

1. **Watcher lifecycles** survive shell exits. `watcher start` from
   one shell keeps watching even after the shell closes.
2. **Hook drivers** are detached into their own session (`setsid`),
   so they survive the CLI exit and `wt create` returns promptly
   regardless of how slow the hooks themselves are.
3. **Snapshot cache** is shared across shells; two terminals
   creating two worktrees on the same branch share the cached
   template DB.

The daemon's socket is 0600 and ownership-checked on every
accept; on Linux via `SO_PEERCRED`, on other platforms via
`stat()` of the socket file.

### Init parity

| Platform | Unit file | Boot helper |
|---|---|---|
| Linux | `~/.config/systemd/user/treemand.service` | `systemctl --user enable --now treemand` |
| macOS | `~/Library/LaunchAgents/dev.stubbe.treemand.plist` | `launchctl bootstrap gui/$UID …` |

`treeman daemon install` writes whichever fits the host and runs
the boot helper. `treeman daemon start` / `stop` / `status` route
through the same init. The CLI falls back to spawning `treemand`
directly when no unit is installed, so transient use without
install also works.

## RPC envelope

`treeman` talks to `treemand` over the unix socket with a line-JSON
protocol (protocol version 2): a request is `{"method": <m>, "<m>":
{…args}}` and every response carries a `kind` field. State mutations
(create/finalize/teardown/prepare/…) don't have their own methods —
they're submitted as a **plan** of tasks through the `run_plan` method
and the daemon executes them, returning `plan_queued` (async) or
`plan_result` (with `wait`).

The full method / response-kind / task / param surface is generated
from the constants in `internal/rpc/rpc.go`:
**[rpc-reference.md](rpc-reference.md)**.

The calling shell's environment rides along on the relevant tasks (via
their params) so hook subprocesses see the user's `$PATH`,
nvm/asdf/rbenv shims, etc.

Every daemon goroutine — accept loop, per-connection handler, watcher
loops, plan lanes, background reapers — runs through `internal/safego`,
which recovers panics so one bad async task can't take down the daemon.

## Development

```sh
just build    # ./bin/treeman + ./bin/treemand with version baked in
just check    # lint (golangci-lint fmt+vet+run) + test + sync-{schema,docs,flake}
just nix-check
just sync-flake [VERSION]
just release-{patch,minor,major}   # tag + push, GH Actions builds + publishes
```

The `sync-flake` recipe rewrites `flake.nix` `vendorHash` and
`version` to match the current `go.sum` / tag. Called automatically
from the release recipes so the flake build never drifts.
