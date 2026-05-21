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

## Features

- **Per-worktree namespaces** for MySQL / MariaDB / TiDB,
  PostgreSQL, MongoDB, Redis (DB-index scoping), Elasticsearch /
  OpenSearch. Snapshot-cache bringup for the RDBMS engines,
  flush-and-ready for the rest.
- **Snapshot cache** with LRU eviction (`cap_per_repo`) — repeated
  `wt create` for the same migrations + dump hits a cached
  template DB and skips the cold rebuild. Native template-copy
  primitives: `CREATE DATABASE … TEMPLATE` on Postgres, table-by-
  table `INSERT … SELECT` on MySQL.
- **Hook groups** — declarative DAG of postcreate / predelete
  commands. Inside a group: sequence. Across groups: parallel.
  Drivers run detached via `setsid` so the CLI returns instantly.
- **Migration framework presets** for Laravel, Rails, Django,
  Flyway, sqlx-cli, diesel, golang-migrate, goose, dbmate, Knex,
  Drizzle, Prisma, TypeORM, mikro-orm — used by `treeman init`
  and `treeman fw detect`. The runtime reads only what's in the
  YAML (see [Fully declarative](#fully-declarative--no-hidden-defaults)).
- **Parallel test runner support** out of the box — `clones: auto`
  detects worker counts from phpunit.xml, pytest-xdist
  `addopts`, Jest `maxWorkers`, vitest `pool.threads`,
  paratest defaults, etc.
- **File watcher** (fsnotify + MySQL binlog tail) for live
  rebuild-or-delta updates as migrations or seed dumps change.
- **`wt switch` / `wt back`** path-printing subcommands so shell
  functions can `cd "$(treeman wt switch foo)"`.
- **JSON Schema generated** from the Go config types via
  `treeman schema dump` — `.treeman.yaml` autocompletes correctly
  in any editor with the YAML language server.
- **Single static binary** per platform — no CGo, no system
  libraries; CI cross-builds `{linux,darwin}` × `{amd64,arm64}`.
- **Daemon init parity** — `treeman daemon install` writes a
  systemd-user unit on Linux and a launchd LaunchAgent plist on
  macOS; `start`/`stop`/`status` route to whichever is present.

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

## CLI reference

| Command | What |
|---|---|
| `treeman init` | Generate a starter `.treeman.yaml` from cwd markers |
| `treeman doctor` | Health-check the local setup (daemon, config, registry drift) |
| `treeman daemon {start,stop,status,install,uninstall}` | Daemon lifecycle |
| `treeman wt {create,delete,list,register,unregister,finalize}` | Worktree lifecycle |
| `treeman wt show <name>` | Per-worktree dossier — state, recent events, hook runs |
| `treeman wt logs <name>` | Tail events scoped to one worktree |
| `treeman wt wait <name>` | Block until the daemon's finalize completes (CI sync primitive) |
| `treeman wt switch <name> [--create]` | Print worktree path (for shell `cd $(…)`) |
| `treeman wt back [--remove]` | Print main repo path; optionally drop clean worktree |
| `treeman prepare` | ensure → dump → migrate → snapshot → replicate |
| `treeman hook run <phase>` | Run a configured hook phase manually |
| `treeman logs {tail,grep,hooks}` | Query the SQLite event log (see flags below) |
| `treeman slug [path]` | Print the slug derived from a worktree path |
| `treeman config {validate,show [--resolved]}` | Config helpers |
| `treeman schema {dump,install}` | JSON Schema for `.treeman.yaml` |
| `treeman fw detect` | List detected migration + test frameworks |
| `treeman completion {bash,zsh,fish,pwsh}` | Print shell completion script |

`logs tail` / `logs grep` share a filter surface:

```sh
treeman logs tail --follow                      # stream new events
treeman logs tail --worktree PROJ-1234          # scope to one worktree
treeman logs tail --level warn --level error    # repeatable
treeman logs tail --since 5m                    # duration or RFC3339 timestamp
treeman logs tail --event-type wt_finalize_done
treeman logs tail --json | jq .                 # machine-readable
treeman logs grep "snapshot cache" --regex
treeman logs grep checksum --search-payload     # match payload_json instead
treeman logs hooks PROJ-1234                    # last N hook_runs for a worktree
```

Source the completion script from your shell rc:

```sh
# bash (~/.bashrc)
source <(treeman completion bash)

# zsh (~/.zshrc)
source <(treeman completion zsh)

# fish (~/.config/fish/completions/treeman.fish)
treeman completion fish > ~/.config/fish/completions/treeman.fish
```

`treeman <cmd> --help` for full flag listings.

### Output, color, paging

treeman prints colored, symbol-prefixed status lines to a TTY and
degrades to plain ASCII when stdout is piped, redirected, or
`NO_COLOR=1` is set. `FORCE_COLOR=1` / `CLICOLOR_FORCE=1` force
colors on even when piping (useful for `treeman ... | less -R`).

Read commands that may produce more than a screen of output
(`treeman logs tail|grep`, `treeman wt show`, `treeman config show`)
auto-page through `$PAGER` (default: `less -FRX` — `-F` quits if the
output fits on one screen, `-R` keeps colors, `-X` skips the
alt-screen). Disable per-invocation with `--no-pager`, or globally
with `TREEMAN_NO_PAGER=1` / `PAGER=`. `--follow` and `--json` always
bypass the pager.

`--json` is supported on `treeman daemon status`, `treeman wt list`,
`treeman slug`, `treeman fw detect`, `treeman logs {tail,grep,hooks}`,
and `treeman doctor` — emits one object (or one per row) suitable
for `jq` consumption.

### Environment variables

| Variable | Effect | Default |
|---|---|---|
| `NO_COLOR` | Disable ANSI color when non-empty. | — |
| `FORCE_COLOR` / `CLICOLOR_FORCE` | Force ANSI color even when stdout is piped. | — |
| `TERM=dumb` | Disable ANSI color regardless of TTY detection. | — |
| `LANG` / `LC_ALL` / `LC_CTYPE` | Non-UTF-8 locale falls back to ASCII symbols (`[ok]`, `[x]`, `->`). | host locale |
| `PAGER` | Pager binary for long output. Set empty to disable. | `less -FRX` |
| `TREEMAN_NO_PAGER=1` | Globally disable paging. | — |
| `XDG_DATA_HOME` | Root for the SQLite event log (`<XDG_DATA_HOME>/treeman/treeman.db`). | `~/.local/share` |
| `XDG_RUNTIME_DIR` | Root for the daemon's unix socket (`<XDG_RUNTIME_DIR>/treeman.sock`). | `~/.cache` fallback |

All variables are read at process start; restart the daemon
(`treeman daemon stop && treeman daemon start`) after changing
`XDG_*` to relocate state.

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
  - engine: mysql                          # mysql|mariadb|tidb|postgres|postgresql|mongodb|redis|elasticsearch|opensearch
    name_template: "myapp_testing_{slug}"
    dump: { path: storage/dumps/seed.sql.gz }
    fanout: 0                              # 0 = safe per-engine default (mysql 4, pg GOMAXPROCS, mongo 6, es 8).
                                           # raise only if the server is provisioned (max_connections bumped, etc.).
    migrations:                            # fully declarative; runtime never re-detects
      framework: laravel                   # label only — fields below are what treeman reads
      migration_dirs:
        - "database/migrations"
      file_globs: ["*.php"]
      lockfiles: ["composer.lock"]
      hash_mode: filename                  # "filename" (cheap) | "checksum" (mutable migrations)
      on_modify: rebuild                   # "rebuild" | "delta"
    test_clones:                           # parallel-test-runner fan-out
      clones: auto                         # auto = detect from phpunit.xml / pyproject / Jest config
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

### Example: per-stack `databases:` blocks

`treeman init` writes these for you when it detects the matching
framework. Copy + paste into the `databases:` array of an existing
`.treeman.yaml` to add a stack.

**Rails + Postgres** (db/migrate, Gemfile.lock invalidates):

```yaml
- engine: postgres
  name_template: "myapp_test_{slug}"
  migrations:
    framework: rails
    migration_dirs: ["db/migrate"]
    file_globs: ["*.rb"]
    lockfiles: ["Gemfile.lock"]
    hash_mode: filename
    on_modify: rebuild
  test_clones:
    clones: auto          # reads parallel_workers from config/test.rb / spec_helper
    name_template: "myapp_test_{slug}_w{n}"
```

**Django + Postgres** (auto-discovered app `migrations/` dirs):

```yaml
- engine: postgres
  name_template: "myapp_test_{slug}"
  migrations:
    framework: django
    migration_dirs: ["**/migrations"]
    file_globs: ["[0-9]*_*.py"]
    lockfiles: ["poetry.lock", "Pipfile.lock", "requirements.txt"]
    hash_mode: filename
  test_clones:
    clones: auto          # reads pytest -n / pytest-xdist config
    name_template: "myapp_test_{slug}_w{n}"
```

**golang-migrate + MySQL**:

```yaml
- engine: mysql
  name_template: "svc_test_{slug}"
  migrations:
    framework: golang-migrate
    migration_dirs: ["migrations", "services/*/migrations"]
    file_globs: ["*.up.sql"]
    lockfiles: ["go.sum"]
    hash_mode: filename
  test_clones:
    clones: 4             # explicit count; Go's `-parallel` is per-package
    name_template: "svc_test_{slug}_w{n}"
```

**sqlx-cli + Postgres** (migrations are mutable — checksum hash):

```yaml
- engine: postgres
  name_template: "app_test_{slug}"
  migrations:
    framework: sqlx-cli
    migration_dirs: ["migrations", "crates/*/migrations"]
    file_globs: ["*.sql"]
    lockfiles: ["Cargo.lock"]
    hash_mode: checksum   # contents hash, not just filenames
    on_modify: delta      # try binlog/diff replay before rebuild
  test_clones:
    clones: auto          # reads cargo nextest config
    name_template: "app_test_{slug}_w{n}"
```

### Templated names

Several config fields are template strings rendered with the
worktree's slug:

| Token | Example |
|---|---|
| `{slug}` | `proj_123` |
| `{slug_dash}` | `proj-123` |
| `{slug_upper}` | `PROJ_123` |
| `{slug_redis_index}` | `7` (deterministic 0–15 hash of slug) |
| `{n}` | test-clone index (1-based) |

### Snapshot cache + GC

Each `prepare` run fingerprints `(engine, engine_version,
source_db, framework, migrations_hash, dump_hash, lockfile_hashes)`
into a SHA-256 key. If a row with that key already exists in the
SQLite `snapshots` table AND the template DB still exists on the
engine, treeman skips the cold rebuild and `CREATE DATABASE …
TEMPLATE` / `INSERT … SELECT`s into the test clones directly.

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
  cached template + test clone, with the schema cache for the
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

### Fully declarative — no hidden defaults

treeman runs against what `.treeman.yaml` says, not what it guesses
from filesystem markers. Init helpers (`treeman init`,
`treeman fw detect`) inspect the repo and write the corresponding
YAML; once written, those fields are authoritative. If a migration
dir lives somewhere unusual, change `migration_dirs` and the next
prepare sees it — no recompile, no rebuild of treeman, no per-
framework code path. The same applies to env files: only what
`env_scoping.sources` lists is read.

The built-in framework presets exist solely as init-time templates
and are listed by `treeman fw detect`. Copy fields in by hand for
custom layouts.

### Credential resolution from .env

treeman reads env files **only when** `env_scoping.sources` is
declared. There is no implicit default — the runtime never reads
files you didn't list. `treeman init` writes a `sources:` block
matching the framework it detected (Laravel → `.env.testing` etc.);
edit the list to suit.

```yaml
env_scoping:
  sources:                # files read in order, last wins
    - .env
    - .env.testing        # baseline for tests
    - .env.testing.local  # per-dev overrides
```

Relative paths resolve against the repo root; absolute paths are
honoured as-is so you can pull from outside the repo (e.g. a
shared secret store).

When `sources` resolves to a non-empty environment, the resolver
fills any unset fields in the `connections:` block. Supported flavours per engine:

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
container, set `container:` (raw container name) or
`compose_service:` (docker-compose service name) on the connection
block. treeman runs `<engine> inspect` and dials whichever of these
works:

1. **Published host port** — `127.0.0.1:<HOST_PORT>` when
   `-p HOST:CONTAINER` / `ports:` publishes the in-container port.
   Cross-platform; works on macOS, Windows, Linux, Colima,
   OrbStack, Docker Desktop, Podman, nerdctl, finch, lima.
2. **Bridge-network IP** — falls back to the container's IP on the
   chosen network (Linux + OrbStack route this directly).
3. **`host.docker.internal`** — final fallback when treeman is
   running *inside* a container and the configured host isn't
   reachable.

Lookups are cached for 30s; a connection failure auto-invalidates
so a container restart settles within one retry.

```yaml
connections:
  mysql:
    container: myapp-mysql        # raw container name (`docker run --name`)
    container_engine: docker      # docker | podman | nerdctl | finch | orbctl | …
    container_network: myapp_net  # optional: pin which network's IP to use
    port: 3306                    # internal port; defaults to engine default
    user: root
    password_env: MYSQL_ROOT_PASSWORD   # or omit — see "Container env" below

  postgres:
    compose_service: db           # docker-compose service name (alternative to container:)
    compose_project: myapp        # defaults to $COMPOSE_PROJECT_NAME if unset
    user: postgres

  mongodb:
    container: myapp-mongo
    uri: "mongodb://placeholder:27017"   # host:port rewritten at dial time
```

**Container env autodiscovery.** When `password_env:` is unset and
the resolver still has no password from env files, treeman pulls
`MYSQL_ROOT_PASSWORD` / `POSTGRES_PASSWORD` straight out of the
container's `Config.Env`. Skip the password block when you've
already declared the secret on the container itself.

**Engines.** Anything that supports `inspect` and `ps --filter
label=...` works — `docker`, `podman`, `nerdctl`, `finch`,
`orbctl`, `lima nerdctl`. Default is `docker`.

**Running treeman inside a devcontainer.** treeman detects
`/.dockerenv`, `/run/.containerenv`, `REMOTE_CONTAINERS=true` and
`DEVCONTAINER=true`. When inside a container, the inspect path is
skipped if the engine socket isn't reachable — your configured
`host:` is used as-is, so on a shared compose network just write
the sibling service name (`host: db`) and it resolves via the
compose-provided DNS. As a last resort `host.docker.internal` is
probed before erroring.

### Running hooks inside a container

postcreate / predelete / postdelete hook entries accept an
`in_container:` (or `compose_service:`) directive that wraps every
step in `<engine> exec` so the command runs inside the named
container rather than on the host. Useful for `composer install`,
`npm ci`, `php artisan migrate`, `bundle install`, … that depend
on the dev container's toolchain.

```yaml
hooks:
  postcreate:
    # Single-step group, in a named container.
    - { run: "composer install", in_container: myapp-php }

    # Multi-step group, in a compose service.
    - compose_service: app
      compose_project: myapp
      steps:
        - "npm ci"
        - { run: "php artisan migrate", cwd: /var/www/html }
```

`step.cwd` becomes `-w <cwd>` on the `exec` call (interpreted
inside the container's filesystem). Leave unset to use the
container's `WORKDIR`.

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
