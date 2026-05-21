# treeman

**Per-worktree development environment helper.** Spin up scoped
databases, search indices, and test artefacts per git worktree;
tear them down on delete; keep them in sync as your migrations or
fixtures change. Works across migration frameworks, test
frameworks, and language stacks — pick a `.treeman.yaml`, run
`treeman wt create branch-name`, get an isolated checkout with a
ready-to-test database stamped to that worktree.

Pure wire-protocol DB access (Go `database/sql` for MySQL +
PostgreSQL, the official Mongo / Redis / Elasticsearch SDKs); no
shelling out to `mysql` / `psql` / `mongosh` / `redis-cli`.
Single user-mode daemon, thin CLI client, SQLite-backed event log.

---

## Why treeman

Git worktrees give every branch its own checkout — but the
checkout alone isn't enough. A real working tree needs:

- a database scoped to that worktree (`myapp_testing_proj_123`)
  so tests don't trample each other
- `N` paratest clones of that database fanning out from a single
  cached template (`CREATE DATABASE … TEMPLATE` on Postgres,
  `INSERT … SELECT` on MySQL)
- the framework's migrations applied
- `.env` / `phpunit.xml` patched to point at the per-worktree DB
- post-create install hooks (composer / yarn / pnpm / go mod /
  cargo / bundler …) running in parallel
- pre-delete teardown that drops the DBs + Redis index + ES
  prefix when you're done with the branch

treeman owns that lifecycle so the prompts you type stay
`treeman wt create FOO` / `treeman wt delete FOO`, while a small
SQLite event log records every step in a queryable way.

## Features

- **Per-worktree DBs** for MySQL/MariaDB/TiDB, PostgreSQL, MongoDB,
  Redis (DB-index scoping), Elasticsearch / OpenSearch — bringup
  cache for the RDBMS engines, teardown for everything
- **Snapshot cache** with LRU eviction (`cap_per_repo`) — repeated
  `wt create` for the same migrations + dump hits a cached template
  and skips the cold rebuild. Native template-copy primitives:
  `CREATE DATABASE … TEMPLATE` on Postgres, table-by-table `INSERT
  … SELECT` on MySQL
- **Hook groups** — declarative DAG of postcreate / predelete
  commands. Inside a group: sequence. Across groups: parallel.
  Drivers run detached via `setsid` so the CLI returns instantly.
- **Migration framework detection** for Laravel, Rails, Django,
  Flyway, sqlx-cli, diesel, golang-migrate, goose, dbmate, Knex,
  Drizzle, Prisma, TypeORM, mikro-orm
- **Test framework detection** for paratest, pest, pytest-xdist,
  Jest, vitest, Go `-p`, Cargo nextest
- **File watcher** (fsnotify + MySQL binlog tail) for live
  rebuild-or-delta updates as migrations or seed dumps change
- **`wt switch` / `wt back`** path-printing subcommands so shell
  functions can `cd "$(treeman wt switch foo)"`
- **JSON Schema generated** from the Go config types via
  `treeman schema dump` — `.treeman.yaml` autocompletes correctly
  in any editor with the YAML language server
- **Single static binary** per platform — no CGo, no system
  libraries; CI cross-builds `{linux,darwin}` × `{amd64,arm64}`
- **Daemon init parity** — `treeman daemon install` writes a
  systemd-user unit on Linux and a launchd LaunchAgent plist on
  macOS; `start`/`stop`/`status` route to whichever is present

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

# 3. Spin up a worktree end-to-end.
treeman wt create proj-123
#   ↳ git worktree add .worktrees/proj-123 -b proj-123 origin/HEAD
#   ↳ symlinks .env (and any worktrees.links targets)
#   ↳ patches .env.testing's DB_DATABASE → my_app_testing_proj_123
#   ↳ runs postcreate hooks (parallel groups, detached)
#   ↳ prepare: ensure_db → load dump → migrate → snapshot → N paratest clones

# 4. Get the path of an existing worktree for `cd` integration:
cd "$(treeman wt switch proj-123)"

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

## CLI reference

| Command | What |
|---|---|
| `treeman init` | Generate a starter `.treeman.yaml` from cwd markers |
| `treeman daemon {start,stop,restart,status,install,uninstall}` | Daemon lifecycle |
| `treeman wt {create,delete,list,register,unregister,finalize}` | Worktree lifecycle |
| `treeman wt switch <name> [--create]` | Print worktree path (for shell `cd $(…)`) |
| `treeman wt back [--remove]` | Print main repo path; optionally drop clean worktree |
| `treeman prepare` | ensure → dump → migrate → snapshot → replicate |
| `treeman hook run <phase>` | Run a configured hook phase manually |
| `treeman logs {tail,grep}` | Query the SQLite event log |
| `treeman slug [path]` | Print the slug derived from a worktree path |
| `treeman config {validate,show [--resolved]}` | Config helpers |
| `treeman schema {dump,install}` | JSON Schema for `.treeman.yaml` |
| `treeman fw detect` | List detected migration + test frameworks |

`treeman <cmd> --help` for full flag listings.

---

## Configuration

treeman reads config in three layers, last-write-wins:

1. `~/.config/treeman/config.yaml` — global connection defaults
2. `.treeman.yaml` — per-repo config (committed)
3. `.treeman.local.yaml` — per-repo overrides (gitignored)

Sample tree of every top-level block (all optional unless marked):

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/stubbedev/treeman/master/schemas/treeman.schema.json

repo:
  name: my-app                    # required for unambiguous slug derivation

worktrees:
  root: .worktrees                # default
  links: [".env"]                 # symlink from main repo into the worktree
  async_create: true              # default — postcreate + prepare detach to daemon
  async_delete: true              # default
  skip_worktree: true             # mark .env.testing skip-worktree after patching

env_scoping:
  files: [".env.testing"]
  skip_worktree: true
  patches:
    - { key: DB_DATABASE,      template: "myapp_testing_{slug}" }
    - { key: DB_TEST_DATABASE, template: "myapp_testing_{slug}" }
    - { key: REDIS_DB,         template: "{slug_redis_index}" }

# The whole `connections:` block is optional — treeman auto-resolves
# credentials from `.env` / `.env.testing` (Laravel `DB_*`,
# Spring-Boot `SPRING_DATASOURCE_*`, MySQL/PG/Redis component vars).
# Declare anything below only when you want to override the auto-
# detected value.
connections:
  mysql:
    host: 127.0.0.1
    port: 3306
    user: root
    password_env: MYSQL_ROOT_PASSWORD     # overrides .env DB_PASSWORD
  postgres:
    host: 127.0.0.1
    port: 5432
    user: postgres
    password_env: PGPASSWORD
  mongodb: { uri: "mongodb://127.0.0.1:27017" }
  redis:   { url: "redis://127.0.0.1:6379" }
  elasticsearch: { url: "http://127.0.0.1:9200" }

databases:
  - engine: mysql                          # mysql|mariadb|tidb|postgres|mongodb|redis|elasticsearch
    name_template: "myapp_testing_{slug}"
    dump: { path: storage/dumps/seed.sql.gz }
    migrations: { framework: laravel }     # see `treeman fw detect`
    paratest:
      clones: auto                         # auto = detect from phpunit.xml / pyproject / etc.
      name_template: "myapp_testing_{slug}_test_{n}"

hooks:
  precreate:                               # synchronous, sequenced, blocks create
    - "git pull --ff-only"
  postcreate:                              # async (parallel groups)
    - composer install --no-interaction --prefer-dist
    - yarn install --frozen-lockfile
    - group:
        - cd frontend && yarn install
        - cd frontend && yarn build:dev
  predelete:                               # async; runs before git worktree remove
    - "echo dropping caches"

snapshots:
  cache_dir: ~/.cache/treeman/snapshots    # only used by GC reports
  retention:
    cap_per_repo: 8                        # NEW: hard cap, LRU evicts on new generation
    max_age_days: 30
    max_total_gb: 50
    gc_interval_minutes: 60                # daemon background sweep

watcher:
  paths:
    - { glob: "database/migrations/**", on: auto }
    - { glob: "storage/dumps/*.sql.gz",  on: rebuild }
  debounce_ms: 500
  binlog:
    enabled: true                          # MySQL only — see Binlog section

daemon:
  socket: $XDG_RUNTIME_DIR/treeman.sock
  log_level: info
```

### Hook groups

Each entry under `postcreate` / `predelete` / `postdelete` is a
**group**. Within a group: commands run in sequence (first
non-zero exit aborts the group). Across groups: groups run in
**parallel**. Each group becomes one `setsid`-detached driver, so
the CLI returns immediately after spawning drivers.

Three forms:

```yaml
hooks:
  postcreate:
    # bare string — one-command group
    - "composer install"

    # map — one-command group with extra fields
    - { run: "yarn build", cwd: frontend, env: { NODE_ENV: production } }

    # sequence — multi-command group, commands chain with &&
    - group:
        - "npm install"
        - "npm run build"
```

`precreate` is the one **synchronous** phase: each entry runs in
order in the foreground and a non-zero exit aborts the worktree
creation. Useful for `git pull`, `git lfs fetch`, etc.

### Templated names

Several config fields are template strings rendered with the
worktree's slug:

| Token | Example |
|---|---|
| `{slug}` | `proj_123` |
| `{slug_dash}` | `proj-123` |
| `{slug_upper}` | `PROJ_123` |
| `{slug_redis_index}` | `7` (deterministic 0–15 hash of slug) |
| `{n}` | paratest clone index (1-based) |

### Snapshot cache + GC

Each `prepare` run fingerprints `(engine, engine_version,
source_db, framework, migrations_hash, dump_hash, lockfile_hashes)`
into a SHA-256 key. If a row with that key already exists in the
SQLite `snapshots` table AND the template DB still exists on the
engine, treeman skips the cold rebuild and `CREATE DATABASE …
TEMPLATE` / `INSERT … SELECT`s into the paratest clones directly.

`snapshots.retention.cap_per_repo` (default `8`) hard-caps how
many cached templates per repo treeman will retain. When the
`(cap+1)`th snapshot is recorded, a background goroutine drops
the LRU template DBs and clears their rows. This keeps engine
disk usage bounded without you having to babysit it.

### Frameworks

`treeman fw detect` lists every framework treeman recognises in
the current repo. Built-in detectors:

| Framework | Marker | Migration dir(s) | Hash mode |
|---|---|---|---|
| Laravel | `artisan` | `database/migrations`, `app/Modules/*/Database/Migrations` | filename |
| Rails | `bin/rails`, `Gemfile`, `config/database.yml` | `db/migrate` | filename |
| Django | `manage.py` | `**/migrations` | filename |
| golang-migrate | `go.mod` | `**/migrations` | filename |
| sqlx-cli | `Cargo.toml`, `migrations/` | `migrations`, `crates/*/migrations` | checksum |
| diesel | `diesel.toml` | `migrations`, `crates/*/migrations` | filename |
| dbmate | `db/migrations` | `db/migrations` | filename |
| Knex | `knexfile.{js,ts}` | `migrations`, `db/migrations` | filename |
| Drizzle | `drizzle.config.{ts,js}` | `drizzle`, `src/drizzle/migrations` | filename |
| Prisma | `prisma/schema.prisma` | `prisma/migrations` | filename |
| TypeORM | `data-source.{ts,js}` | `src/migrations`, `migrations` | filename |
| mikro-orm | `mikro-orm.config.*` | `src/migrations` | filename |
| Flyway | `flyway.{conf,toml}` | `db/migration`, `src/main/resources/db/migration` | checksum |
| goose | `dbmate`-style with `.sql` | `db/migrations` | filename |

`HashFilename` mode skips file IO (Laravel/Rails/Django don't
mutate migrations; new files alone change the hash). `HashChecksum`
hashes contents (sqlx-cli/Flyway mutate in place).

### Binlog (MySQL delta replay)

When `watcher.binlog.enabled: true` and the MySQL server runs
with `binlog_format=ROW`, `binlog_row_image=FULL`,
`binlog_row_metadata=FULL` (5.7+/8.0+), the daemon tails the
binary log from a checkpointed position and applies events to
each cached template + clone in sequence, instead of cold-
rebuilding from the dump every time a migration runs.

- **DDL** replay (default on, `apply_ddl: true`): every CREATE /
  ALTER / DROP that lands on the source DB is mirrored to every
  cached template + paratest clone, with the schema cache for the
  source invalidated so subsequent DML events re-resolve columns.
- **DML** replay (default off, `apply_dml: true`): WriteRows /
  UpdateRows / DeleteRows events are reconstructed as parameterised
  INSERT / UPDATE / DELETE per target. PK-based WHERE when the
  table has one; full-row NULL-safe (`<=>`) match as a fallback.
  Off by default because wrong-row replay is recoverable only from
  a cold rebuild — opt in only when the source DB is dev-only and
  reseeding is cheap.

The watcher dispatches `delta` vs `rebuild` per-framework: any
`HashChecksum` framework or `on: rebuild` watcher path forces a
full rebuild; anything else replays the binlog.

### Credential resolution from .env

treeman reads `<repo>/.env`, `.env.local`, `.env.test`,
`.env.testing` (+ their `.local` variants) and uses them to fill
the connection config when the YAML omits a block or leaves a
field unset. Supported flavours per engine:

| Engine | Env vars treeman reads (first non-empty wins) |
|---|---|
| MySQL | `DB_TEST_*` → `DB_*` (Laravel); `MYSQL_URL`; `SPRING_DATASOURCE_URL` (jdbc:mysql://…) + `SPRING_DATASOURCE_{USERNAME,PASSWORD}`; `MYSQL_PASSWORD` / `MYSQL_PWD` |
| Postgres | `DB_TEST_*` → `DB_*` (Laravel); `DATABASE_URL`; `PG*` (`PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`); `SPRING_DATASOURCE_URL` (jdbc:postgresql://…) |
| MongoDB | `MONGODB_URI` / `MONGO_URL` / `DATABASE_URL`; `MONGO_DB_HOST` + `MONGO_DB_PORT` composed; `SPRING_DATA_MONGODB_*` |
| Redis | `REDIS_URL`; `REDIS_HOST` + `REDIS_PORT` + `REDIS_PASSWORD` composed; literal `null` / `(null)` passwords skipped |
| Elasticsearch | `ELASTICSEARCH_URL` / `ELASTICSEARCH_HOSTS` (auto-prefixes `http://` if missing) |

Practical effect: a Laravel repo whose `.env.testing` carries the
canonical `DB_HOST=mysql / DB_USERNAME=root / DB_PASSWORD=secret`
needs **no** `connections:` block in `.treeman.yaml`. The same
goes for Spring-Boot apps with `SPRING_DATASOURCE_URL`, etc.

YAML always wins where present — declare a field only when you
want to override the env value.

### Connecting to DBs in a container

If your MySQL / Postgres / Mongo / Redis / Elasticsearch runs in a
docker (or podman) container with **no published port**, set
`container:` on the connection block. treeman runs `<engine>
inspect` to read the container's bridge-network IP and dials that
directly instead of the configured `host`. The lookup is cached
for 30s; a connection failure auto-invalidates so a container
restart with a new IP settles within one retry.

```yaml
connections:
  mysql:
    container: kontainer-mysql    # required: container name
    container_engine: docker      # optional, default "docker"
    port: 3306                    # optional when container exposes it
    user: root
    password_env: MYSQL_ROOT_PASSWORD
  mongodb:
    container: kontainer-mongo
    uri: "mongodb://placeholder:27017"   # host part rewritten at dial time
```

Works out of the box on Linux because the host can route to the
docker bridge network. On macOS / Windows Docker Desktop, prefer
publishing the port (`-p` or `ports:` in compose) — the container
IP isn't reachable from the host.

---

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

---

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

---

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

---

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

---

## License

Dual-licensed under Apache-2.0 OR MIT.
