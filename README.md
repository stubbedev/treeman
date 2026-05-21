# treeman

**Per-worktree development environment helper.** Spin up isolated
databases, search indices, and parallel test clones per git
worktree; tear them down on delete; keep them in sync as your
migrations or fixtures change. Language- and framework-agnostic —
runs the same way against a Laravel + MySQL repo, a Rails +
Postgres repo, a Django + Postgres repo, a Go + golang-migrate
service, or a Rust + sqlx workspace.

Pure wire-protocol DB access (Go `database/sql` for MySQL +
PostgreSQL, the official Mongo / Redis / Elasticsearch SDKs); no
shelling out to `mysql` / `psql` / `mongosh` / `redis-cli`.
Single user-mode daemon, thin CLI client, SQLite-backed event log.

---

## Why treeman

Git worktrees give every branch its own checkout — but the
checkout alone isn't enough. A working tree needs:

- a database scoped to that worktree (e.g. `myapp_test_proj_123`)
  so parallel branches don't trample each other's data
- `N` test-runner clones of that database fanning out from a
  single cached template, so the project's parallel test runner
  (paratest, pest, pytest-xdist, Jest workers, Go `-parallel`,
  cargo nextest, …) gets a fresh DB per worker
- the project's migrations applied to the source DB
- `.env`-style config (or `phpunit.xml`, `pyproject.toml`, etc.)
  patched to point at the per-worktree DB names
- post-create install hooks (composer / yarn / pnpm / go mod /
  cargo / bundler / pip …) running in parallel
- pre-delete teardown that drops every per-worktree namespace
  (DB, Redis index, ES index prefix) when you're done

treeman owns that lifecycle. `treeman wt create FOO` /
`treeman wt delete FOO` are the only commands you type; a SQLite
event log records every step.

## At a glance

- **Per-worktree namespaces** for MySQL / MariaDB / TiDB,
  PostgreSQL, MongoDB, Redis (DB-index scoping), Elasticsearch /
  OpenSearch.
- **Snapshot cache** with LRU eviction — repeated `wt create` on
  the same migrations + dump hits a cached template DB.
- **Hook groups** — declarative DAG of postcreate / predelete
  commands. Within a group: sequence. Across groups: parallel.
- **Parallel test runner support** — `clones: auto` detects
  worker counts from phpunit.xml, pytest-xdist, Jest, vitest,
  paratest, cargo nextest, etc.
- **File watcher** (fsnotify + MySQL binlog tail) for live
  rebuild-or-delta as migrations or seed dumps change.
- **MCP server** — `treeman mcp` lets Claude Code / Claude
  Desktop / Cursor drive the lifecycle over stdio.
- **Single static binary** per platform — no CGo, no system
  libraries; CI cross-builds `{linux,darwin}` × `{amd64,arm64}`.

See [docs/](docs/) for the deep dives — CLI reference,
configuration schema, AI integration, internals.

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
- **Go 1.23+** — only if building from source.

That's the whole dependency list. No Python, no Node, no
language-specific tooling required.

---

## Install

Prebuilt tarballs for every tagged release:

```sh
# linux/amd64 example — substitute the platform you need
curl -L -o /tmp/treeman.tgz \
  https://github.com/stubbedev/treeman/releases/latest/download/treeman-1.0.1-linux-amd64.tar.gz
tar -xzf /tmp/treeman.tgz -C /tmp
install /tmp/treeman-*-linux-amd64/treeman  ~/.local/bin/
install /tmp/treeman-*-linux-amd64/treemand ~/.local/bin/
```

From source (Go 1.23+):

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
#   ↳ symlinks .env (and any worktrees.links targets)
#   ↳ patches env_scoping.files entries (.env.testing, settings.py,
#     phpunit.xml, etc.) to point at per-worktree DB names
#   ↳ runs postcreate hooks (parallel groups, detached)
#   ↳ prepare: ensure_db → load dump → migrate → snapshot → N test clones

# 4. Get the path of an existing worktree for `cd` integration:
cd "$(treeman wt switch proj-123)"

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

A ready-to-source zsh shim that wraps `wt switch` / `wt back` for
`cd`-into-worktree UX lives at `contrib/shim.zsh`:

```sh
# In ~/.zshrc:
source /path/to/treeman/contrib/shim.zsh

# Then:
wt proj-123          # cd into existing worktree (or report missing)
wt proj-123 -c       # create + cd to new worktree
wt new proj-123      # same as `wt proj-123 -c`
wt -                 # cd back to main repo
wt - --remove        # cd back + drop current worktree if clean
wt list              # passthrough to `treeman wt list`
```

---

## Documentation

| Page | What you'll find |
|---|---|
| [docs/cli.md](docs/cli.md) | Full command reference, log filters, completion, output/color/paging, env vars |
| [docs/configuration.md](docs/configuration.md) | `.treeman.yaml` reference — every block, per-stack examples, templated names, hooks, credential resolution, container DBs + hooks |
| [docs/advanced.md](docs/advanced.md) | Snapshot cache + GC, framework presets, MySQL binlog delta replay |
| [docs/mcp.md](docs/mcp.md) | MCP / AI integration — Claude Code, Claude Desktop, Cursor, security model |
| [docs/internals.md](docs/internals.md) | Storage layout, daemon model, init parity, RPC envelope, development |

---

## License

Dual-licensed under Apache-2.0 OR MIT.
