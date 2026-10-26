# Internals — daemon, RPC, storage

## Scope and non-goals

These boundaries are deliberate design decisions, not gaps. Each is
enforced in code at the named place:

1. **Git-worktree-only VCS.** Every git subprocess is funneled through
   `internal/gitcmd` (which explains why no Go git library is used),
   parsing helpers live in `internal/gitx`, and worktree creation goes
   through `wt.CreateInStore` → `git worktree add`. Treeman requires
   git to be installed anyway (it manages git worktrees), so Jujutsu,
   Mercurial, and Sapling are non-goals. Multi-VCS support would mean
   abstracting `gitcmd` + `gitx` + `wt/create` behind a VCS interface
   and is deliberately out of scope.
2. **Single-user daemon.** One human per host: the socket checks the
   peer's uid (SO_PEERCRED on Linux, owner stat elsewhere) and the
   SQLite store assumes one writer identity.
3. **One host.** The daemon, its socket, the registry DB, and the
   databases it prepares are all local; there is no remote-daemon or
   fleet coordination layer.
4. **Unix-like OS.** Unix sockets, `setsid` hook detachment, and
   systemd-user/launchd units are the process model; Windows lacks
   all three natively.

If you find an issue thread asking "would treeman work with $OTHER_VCS
/ multi-user / remote daemons?", link here instead of re-deriving the
answer.

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
   so they survive the CLI exit and `worktree create` returns promptly
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
from the constants in `pkg/rpc/rpc.go`:
**[rpc-reference.md](rpc-reference.md)**.

The calling shell's environment rides along on the relevant tasks (via
their params) so hook subprocesses see the user's `$PATH`,
nvm/asdf/rbenv shims, etc.

Every daemon goroutine — accept loop, per-connection handler, watcher
loops, plan lanes, background reapers — runs through `pkg/safego`,
which recovers panics so one bad async task can't take down the daemon.

## Adding an engine

The per-family switches live in ONE place: the driver registry in
`internal/db/engineconn` (`registry.go` + the explicit production list
in `registry_drivers.go`). Everything else — `engineconn.Connect`,
`engineconn.Configured`, prepare, teardown, GC, and stale-worktree
recovery — is registry-driven and picks a new engine up for free.

Checklist for a new engine family (≤4 touchpoints, #47):

1. **Driver package** `internal/db/<engine>`: dial + the `Conn`
   primitives (Drop/Exists/ListMatching/DropMatching/…).
2. **Registry entry** in `engineconn/registry_drivers.go`: a
   `Configured` field-check and a `Connect` dialer.
3. **Config**: a `ConnectionsConfig` field, a `databases[].engine`
   alias pair in `engine.Known` + `engine.Canonical`, and a
   `validate.go` rule if the family can't support some modes.
4. **Extras** (only what applies): a patcher driver if config files
   need rewriting, an `initgen` preset if a framework pairs with it,
   docker-tier e2e under `e2e/<engine>`.

The jsonschema `engine` enum is GENERATED from `engine.Known`
(`just sync-schema`) — adding an alias cannot half-land. History:
the pre-registry switches drifted (the GC once rejected `mongodb`
while prepare accepted it), which is what this table exists to
prevent.

## Development

```sh
just build    # ./bin/treeman + ./bin/treemand with version baked in
just check    # lint (golangci-lint fmt+vet+run) + test + sync-{schema,docs,flake}
just nix-check
just sync-flake [VERSION]
just release-{patch,minor,major}   # tag + push, GH Actions builds + publishes
```

The `sync-flake` recipe rewrites `nix/package.nix` `vendorHash` and
`version` to match the current `go.sum` / tag. Called automatically
from the release recipes so the flake build never drifts.
