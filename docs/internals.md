# Internals — daemon, RPC, storage

[← back to README](../README.md)

## Storage layout

| Path | What |
|---|---|
| `~/.local/share/treeman/treeman.db` | SQLite event log + worktree registry + snapshots table |
| `~/.local/share/treeman/treemand.log` | Daemon stderr |
| `$XDG_RUNTIME_DIR/treeman.sock` | JSON-line RPC socket (SO_PEERCRED on Linux, stat-based owner check elsewhere) |
| `~/.config/systemd/user/treemand.service` | systemd-user unit (Linux) |
| `~/Library/LaunchAgents/dev.stubbe.treemand.plist` | launchd LaunchAgent (macOS) |
| `<worktree>/.treeman-hooks/<phase>-<n>.log` | Per-hook driver stdout/stderr |
| `<repo>/schemas/treeman.schema.json` | JSON Schema (only present after `treeman schema install`) |

The store schema lives at `internal/store/migrations/0001_init.sql`
and is shipped embedded into the binary, so a fresh `treeman.db`
self-migrates on first daemon start.

## Daemon model

`treemand` is the long-running process; `treeman` is a thin RPC
client that round-trips JSON over the unix socket. Why a daemon:

1. **Watcher lifecycles** survive shell exits. `watcher start` from
   one shell keeps watching even after the shell closes.
2. **Hook drivers** are detached and parented to PID 1 (`setsid`),
   so `wt create` returns in <2s regardless of how slow the hooks
   themselves are.
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

The line-JSON protocol is documented in `internal/rpc/rpc.go`.
Methods:

| Method | Args | Response |
|---|---|---|
| `ping` | — | `{ kind: "pong" }` |
| `status` | — | `{ kind: "status", daemon_version, pid, watcher_count }` |
| `repo_register` | `{ path }` | `{ kind: "repo_registered", repo_id }` |
| `worktree_finalize` | `{ repo_path, worktree_path, slug, inherited_env }` | `{ kind: "worktree_finalize_queued" }` |
| `worktree_teardown` | `{ repo_path, worktree_path, force, inherited_env }` | `{ kind: "worktree_teardown_queued" }` |
| `watcher_start` / `watcher_stop` / `watcher_list` | `{ repo_path }` | `{ kind: "watcher_*" }` |
| `shutdown` | — | `{ kind: "shutdown_acked" }` |

The `inherited_env` field carries the calling shell's environment
to the daemon so hook subprocesses see the user's `$PATH`,
nvm/asdf/rbenv shims, etc.

## Development

```sh
just build    # ./bin/treeman + ./bin/treemand with version baked in
just check    # gofmt + go vet + go test
just nix-check
just sync-flake [VERSION]
just release-{patch,minor,major}   # tag + push, GH Actions builds + publishes
```

The `sync-flake` recipe rewrites `flake.nix` `vendorHash` and
`version` to match the current `go.sum` / tag. Called automatically
from the release recipes so the flake build never drifts.
