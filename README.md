# treeman

**Per-worktree development environment helper.** Spin up isolated
databases, search indices, and parallel test clones for each git
worktree — torn down on delete, kept in sync as your migrations and
fixtures change. Language- and framework-agnostic.

Pure wire-protocol DB access (Go `database/sql` for MySQL +
PostgreSQL, the official Mongo / Redis / Elasticsearch SDKs) — no
shelling out to `mysql` / `psql` / `mongosh` / `redis-cli`. A single
user-mode daemon, a thin CLI client, and a SQLite-backed event log.

---

## Why treeman

A git worktree gives every branch its own checkout — but a working
tree also needs its own database, `N` test-runner clones fanning out
from a cached template, migrations applied, `.env`-style config patched
to the per-worktree DB names, install hooks run, and teardown that drops
every namespace on delete. treeman owns that lifecycle:
`treeman wt create FOO` and `treeman wt delete FOO` are the only
commands you type.

## Supported engines

| Engine | Variants | Per-worktree isolation |
|---|---|---|
| MySQL | MariaDB, TiDB | Separate database |
| PostgreSQL | — | Separate database |
| MongoDB | — | Separate database |
| Redis | Valkey, DragonflyDB | Key-prefix in DB 0 (cluster-safe, no 16-DB cap) |
| Elasticsearch | OpenSearch | Index-name prefix |

Variants are first-class aliases — declare `engine: mariadb`, `tidb`,
`valkey`, `dragonfly`, or `opensearch` and treeman routes it to the
parent engine's driver (Valkey/DragonflyDB ride the Redis wire
protocol).

## Features

- **Snapshot cache** — repeated `wt create` on the same migrations +
  dump hits a cached template DB; LRU eviction with a per-repo cap
  (default 8), per-source retention, age + size sweeps bound disk use;
  `treeman doctor` flags (and `--fix` reclaims) orphaned templates
  left behind by crashes.
- **Spare-clone pre-warming** — `databases[].prewarm: N` (Postgres)
  keeps N clones pre-restored from the cached template; a cache-hit
  `wt create` claims one via `ALTER DATABASE … RENAME` in milliseconds
  regardless of database size, and the pool refills in the background.
- **Hook lifecycle** — declarative create / delete / checkout /
  file-change trigger lists; actions in a list run in parallel, the
  `run:` steps within an action chain sequentially.
- **Parallel test runner support** — `clones: auto` detects the test
  framework (paratest, pytest-xdist, jest / vitest, cargo-nextest, …)
  and pre-warms one clone per CPU when that runner parallelizes
  per-worker, else one.
- **File watcher** — fsnotify re-fingerprints affected databases on
  input edits and picks `auto | delta | rebuild`.
- **MCP server** — `treeman mcp` exposes config authoring/validation,
  event-log + hook-output queries, live engine state, and snapshot
  inspection to Claude Code / Desktop / Cursor.
- **Desktop notifications** — opt-in `notifications:` block fires
  `notify-send` (Linux) / native banners (macOS) when a worktree turns
  ready, fails, or starts/finishes preparing; configurable per status
  bucket, hot-reloaded without a restart.
- **Single static binary** per platform — no CGo, no system libraries;
  CI cross-builds `{linux,darwin}` × `{amd64,arm64}`.

See [docs/](docs/) for the deep dives — CLI reference, configuration
schema, AI integration, internals.

---

## Requirements

- **git** — `git worktree` is the substrate treeman manages.
- **One of**: `systemd --user` (Linux) or `launchd` (macOS) for
  auto-start. Without one, `treeman daemon start` still works;
  the daemon just won't survive a reboot.
- **Your database server(s)** — treeman manages namespaces on
  existing MySQL / Postgres / Mongo / Redis / Elasticsearch
  instances; it does not run them. Local server, remote server,
  or container-hosted server (auto-discovered via `docker
  inspect` / `compose ps`) all work.
- **Go 1.25+** — only if building from source.

That's the whole dependency list. No Python, no Node, no
language-specific tooling required.

---

## Install

Via Homebrew (macOS + Linux):

```sh
brew install stubbedev/treeman/treeman
```

Prebuilt tarballs for every tagged release. The asset filename
embeds the version, so resolve the latest tag first:

```sh
# linux/amd64 example — substitute the platform (darwin-amd64,
# darwin-arm64, linux-arm64) you need.
VER=$(curl -fsSL https://api.github.com/repos/stubbedev/treeman/releases/latest | grep '"tag_name":' | cut -d'"' -f4 | sed 's/^v//')
curl -L -o /tmp/treeman.tgz \
  "https://github.com/stubbedev/treeman/releases/download/v${VER}/treeman-${VER}-linux-amd64.tar.gz"
tar -xzf /tmp/treeman.tgz -C /tmp
install /tmp/treeman-*-linux-amd64/treeman  ~/.local/bin/
install /tmp/treeman-*-linux-amd64/treemand ~/.local/bin/
```

From source (Go 1.25+):

```sh
git clone https://github.com/stubbedev/treeman
cd treeman
just install        # → $GOBIN/treeman + $GOBIN/treemand
```

Via Nix (flake):

```sh
nix profile install github:stubbedev/treeman
```

Shell completion (optional but recommended):

```sh
source <(treeman completion bash)    # or zsh / fish / pwsh
```

---

## Quick start

```sh
# 1. Bootstrap a config in your repo. treeman init detects the
#    package manager + framework markers and emits a tailored
#    .treeman.yaml.
cd ~/code/my-app
treeman init

# 2. Install + start the user-mode daemon (one-time).
treeman daemon install      # systemd --user on Linux, launchd on macOS
treeman daemon start        # idempotent — uses systemctl/launchctl when installed

# Sanity check whenever something feels off:
treeman doctor              # probes daemon, config, schema, git ↔ registry drift

# 3. Spin up a worktree end-to-end.
treeman wt create proj-123
#   ↳ git worktree add .worktrees/proj-123 -b proj-123 origin/HEAD
#   ↳ brings in worktrees.copies (e.g. .env) + worktrees.links (e.g. node_modules)
#   ↳ patches `patches:` entries (.env.testing, settings.py,
#     phpunit.xml, etc.) to point at per-worktree DB names
#   ↳ runs create hooks (parallel groups, detached)
#   ↳ prepare: ensure_db → load dump → migrate → snapshot → N test clones

# 4. Get the path of an existing worktree for `cd` integration:
cd "$(treeman wt go proj-123)"

# (CI flow: block on the daemon's finalize before running tests.)
treeman wt wait proj-123                  # exits 0 on success, non-zero on failure
treeman wt show proj-123                  # dossier + recent events + hook runs

# 5. Done with the branch:
treeman wt delete proj-123
#   ↳ runs predelete hook (DB drops, FLUSHDB, ES index delete)
#   ↳ git worktree remove

# 6. Cd back to the main checkout (with optional auto-remove if clean):
cd "$(treeman wt back --remove)"
```

A ready-to-source zsh shim that wraps `wt go` / `wt back` for
`cd`-into-worktree UX lives at `contrib/tm.zsh` (exposes a `tm`
shell function):

```sh
# In ~/.zshrc:
source /path/to/treeman/contrib/tm.zsh

# Then:
tm proj-123          # cd into existing worktree (or report missing)
tm proj-123 -c       # create + cd to new worktree
tm new proj-123      # same as `tm proj-123 -c`
tm -                 # cd back to main repo
tm - --remove        # cd back + drop current worktree if clean
tm list              # passthrough to `treeman wt list`
```

---

## Documentation

| Page | What you'll find |
|---|---|
| [docs/cli.md](docs/cli.md) | Full command reference, log filters, completion, output/color/paging, env vars |
| [docs/configuration.md](docs/configuration.md) | `.treeman.yaml` guide — every block, per-stack examples, templated names, hooks, credential resolution, container DBs |
| [docs/config-reference.md](docs/config-reference.md) | Generated field-by-field `.treeman.yaml` reference (from the Go types) |
| [docs/advanced.md](docs/advanced.md) | Snapshot cache + GC, framework presets |
| [docs/mcp.md](docs/mcp.md) | MCP / AI integration — Claude Code, Claude Desktop, Cursor, security model |
| [docs/mcp-tools.md](docs/mcp-tools.md) | Generated MCP tool + prompt reference (from the registry) |
| [docs/events.md](docs/events.md) | Generated event-type reference (from the `store.Evt*` constants) |
| [docs/frameworks.md](docs/frameworks.md) | Generated migration-framework preset table (from the detector registry) |
| [docs/rpc-reference.md](docs/rpc-reference.md) | Generated RPC method / task / kind reference (from `internal/rpc`) |
| [docs/internals.md](docs/internals.md) | Storage layout, daemon model, init parity, RPC envelope, development |

---

## License

Dual-licensed under Apache-2.0 OR MIT.
