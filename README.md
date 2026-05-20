# treeman

**Per-worktree DB orchestrator with file watcher.** Spin up scoped test
databases per git worktree, tear them down on delete, keep them in sync as
migrations change.

Pure wire-protocol DB access (sqlx / mysql_async / mongodb / redis / reqwest);
no `mysql` / `psql` / `mongosh` / `redis-cli` / `docker exec` shell-out.
Single global daemon, thin CLI client, SQLite-backed event log.

---

## Features

- **23 stateful resources** — relational (MySQL, MariaDB, TiDB,
  PostgreSQL, CockroachDB, SQLite, DuckDB), analytics (ClickHouse,
  InfluxDB), document (MongoDB), KV (Redis, Memcached, etcd), search
  (Elasticsearch, OpenSearch, Meilisearch, Typesense), vector (Qdrant,
  Weaviate, Milvus), graph (Neo4j), queues (RabbitMQ vhosts, NATS
  JetStream, Kafka via REST proxy), object store (S3 / MinIO —
  experimental).
- **`DATABASE_URL` + dotenv-aware** — `connections.*` is optional; falls
  back through per-engine `*_URL` env vars, generic `DATABASE_URL`, and
  the repo's `.env`/`.env.testing`/`.env.test.local` (Laravel / Symfony /
  Node / Cargo conventions). `treeman config show --resolved` shows
  every connection with its provenance.
- **JSON-schema completions** — `treeman schema install` writes the
  global + per-repo schemas to `~/.config/treeman/schemas/` and emits
  the `yaml-language-server: $schema=…` modeline that lights up
  autocompletion in any LSP-backed editor (VS Code via redhat.vscode-
  yaml, Neovim, Helix, Zed). `treeman init` stamps the modeline at the
  top of every generated `.treeman.yaml`.
- **14 migration frameworks auto-detected** — Laravel (incl. nwidart-style
  modules), Rails (incl. engines), Django, golang-migrate, sqlx-cli, Diesel,
  Prisma, Knex, Alembic, Flyway, TypeORM, Drizzle, Sequelize, MikroORM.
- **Test framework auto-detected** — paratest, pest, pytest-xdist,
  pytest-parallel, parallel_tests, jest, vitest, playwright,
  mocha-parallel-tests, cargo-nextest, Maven Surefire, Gradle, plus the
  shared-DB defaults (phpunit, plain pytest, rspec, mocha, cargo test,
  go test, dotnet test). Replication count (per-worker vs shared) is
  derived from detection — no explicit subcommand.
- **Multi-module / monorepo aware** — modules created mid-watch are picked
  up without restart (dynamic re-watch).
- **Per-worktree fan-out** — the daemon watches `.git/worktrees/` itself, so
  every linked worktree (existing, newly added, moved, pruned, or created
  outside treeman) gets its own framework watcher with its own slug. No
  polling, no CLI ↔ daemon coupling, sub-second latency.
- **Survives daemon restarts** — `treemand` auto-resumes watchers for
  every registered repo at boot. `systemctl restart treemand` regains
  coverage with zero manual `treeman watcher start` calls.
- **Config hot-reload** — edits to `.treeman.yaml`,
  `.treeman.local.yaml`, or `~/.config/treeman/config.yaml` trigger the
  daemon to restart that repo's watcher with the fresh config.
- **Snapshot/template cache** — first `prepare` builds; subsequent worktrees
  for the same migration set restore in seconds. Postgres uses native
  `CREATE DATABASE … TEMPLATE`; MySQL uses cross-DB `INSERT … SELECT`.
- **Snapshot GC** — daemon periodically evicts unused templates per
  `snapshots.retention.*` policy (`keep_per_source`, `max_age_days`,
  `max_total_gb`) and DROPs them from the engine. Cadence configurable
  via `gc_interval_minutes`.
- **Replication fan-out** — clones the template into N per-worker test
  DBs (count auto-derived from the detected test framework) so
  `php artisan test --parallel` / `pytest -n auto` / `jest` / etc.
  just work.
- **Declarative YAML config** — global `~/.config/treeman/config.yaml` +
  per-repo `.treeman.yaml` (JSON-schema validated; emit the schema with
  `treeman schema dump`).
- **Single daemon** — owns connection pools + file watchers; CLI is a thin
  unix-socket client (SO_PEERCRED uid check; mode 0600).
- **SQLite event log** — every hook run, watcher event, snapshot build,
  and DB op is queryable via `treeman logs grep`.

---

## Install

### From source (current)

```sh
git clone https://github.com/stubbe/treeman
cd treeman
cargo install --path crates/treeman-cli
cargo install --path crates/treeman-daemon
```

### Nix flake

```sh
nix run github:stubbe/treeman -- --help          # ephemeral
nix profile install github:stubbe/treeman        # persistent
```

### Pre-built binaries

Each git tag (`vX.Y.Z`) ships Linux x86_64 + aarch64 and macOS x86_64 +
aarch64 tarballs as GitHub release assets. See the
[releases page](https://github.com/stubbe/treeman/releases).

---

## Quick start

```sh
# 1. Bootstrap config in your repo.
cd ~/code/my-laravel-app
treeman init               # writes .treeman.yaml from detected framework

# 2. Ensure the daemon is running.
treeman daemon install     # systemd --user (Linux) or LaunchAgent (macOS)
# … or for a one-shot session:
treeman daemon start       # service if installed, else spawn detached

# 3. Create a worktree end-to-end.
treeman wt create PROJ-123
#   ↳ git worktree add ../my-laravel-app-worktrees/PROJ-123 -b PROJ-123 main
#   ↳ symlinks .env, .env.testing, justfile, etc.
#   ↳ patches .env.testing's DB_DATABASE to my-laravel-app_testing_proj_123
#   ↳ runs postcreate hooks
#   ↳ prepare: ensure_db → load dump → migrate → snapshot → N paratest clones

# 4. Tell the daemon to watch for migration changes.
treeman watcher start

# 5. Work in the worktree. New migration files trigger delta updates;
#    edits to existing migrations trigger a wipe+reseed (cached against
#    fingerprint so repeats are fast).

# 6. Done with the ticket.
treeman wt delete PROJ-123
#   ↳ predelete hook
#   ↳ drops scoped mysql DBs + mongo DBs + flushes scoped redis db indices
#     + deletes elasticsearch index prefix
#   ↳ git worktree remove
```

---

## CLI reference

Run `treeman --help` for the full tree. Highlights:

| Command | What it does |
|---|---|
| `treeman init` | Generate `.treeman.yaml` from detected framework |
| `treeman daemon {start,stop,restart,status,install,uninstall}` | Daemon lifecycle (systemd --user on Linux, launchd LaunchAgent on macOS) |
| `treeman wt {create,delete,list,register,unregister}` | Worktree lifecycle |
| `treeman watcher {start,stop,list,worktrees}` | Daemon-managed file watcher (`list` shows repos + worktree counts, `worktrees <repo>` lists the linked worktrees being watched) |
| `treeman watch` | Foreground CLI watcher (debugging) |
| `treeman hook run <phase>` | Run a configured hook phase |
| `treeman prepare` | ensure → dump → migrate → snapshot → replicate |
| `treeman snapshot {list,show,gc}` | Snapshot cache management |
| `treeman db {drop,flush,list}` | Direct DB driver ops |
| `treeman fw detect` | Show detected migration + test frameworks |
| `treeman logs {tail,grep}` | Query the SQLite event log |
| `treeman slug [path]` | Print the slug derived from a worktree path |
| `treeman config {validate,show}` | Config helpers |
| `treeman schema dump` | JSON Schema for `.treeman.yaml` |
| `treeman completions <shell>` | Emit shell completions (bash/zsh/fish/…) |
| `treeman manpage` | Emit roff(7) for `man treeman` |

Short flags everywhere (`-r` repo, `-w` worktree, `-f` force, `-e` engine,
`-n` limit, `-l` level, …). Env-var defaults: `TREEMAN_REPO`,
`TREEMAN_WORKTREE`, `TREEMAN_DB_PATH`, `TREEMAN_SOCKET`.

Aliases: `wt` ↔ `worktree`, `fw` ↔ `frameworks`, `snap` ↔ `snapshot`,
`log` ↔ `logs`, `wt new` ↔ `wt create`, `wt rm` ↔ `wt delete`,
`db rm` ↔ `db drop`, `db ls` ↔ `db list`, etc.

---

## Configuration

Layered: global `~/.config/treeman/config.yaml` → per-repo `.treeman.yaml`
→ per-repo `.treeman.local.yaml` (gitignored). Later layers override.

Full schema (emit JSON Schema with `treeman schema dump`):

```yaml
# Global
daemon:
  socket: $XDG_RUNTIME_DIR/treeman.sock
  log_level: info
  db_log_path: ~/.local/state/treeman/treeman.db

connections:
  mysql:
    host: 127.0.0.1
    port: 3306
    user: root
    password_env: MYSQL_PWD
    pool_max: 8
  postgres:
    host: 127.0.0.1
    port: 5432
    user: postgres
    password_env: PGPASSWORD
    pool_max: 8
  mongodb:
    uri: mongodb://localhost:27017
  redis:
    url: redis://localhost:6379
  elasticsearch:
    url: http://localhost:9200

snapshots:
  cache_dir: ~/.cache/treeman/snapshots
  retention:
    keep_per_source: 500       # most-recent N templates per (engine, source_db)
    max_age_days: 30           # drop templates idle longer than this
    max_total_gb: 50           # cap total catalog-tracked size
    gc_interval_minutes: 60    # daemon GC cadence (floor-clamped to 5)

# Per-repo (.treeman.yaml)
repo:
  name: myapp

slug:
  ticket_regex: "^([A-Z]+)-(\\d+)"
  fallback: "wt_{shorthash8}"

worktrees:
  # Relative paths resolve from the main repo root; absolute paths are
  # used as-is. Branch slashes are preserved, so a branch named
  # `feature/FOO-123-thing` lands at `<root>/feature/FOO-123-thing`.
  # If `root` resolves inside the repo, `treeman wt create` ensures the
  # directory is `.gitignore`d.
  root: .worktrees
  links:
    - .env
    - .env.testing
    - justfile

env_scoping:
  files:
    - .env.testing
    - phpunit.xml
  skip_worktree: true
  patches:
    - key: DB_TEST_DATABASE
      template: "myapp_testing_{slug}"
    - key: MONGO_DB_DATABASE
      template: "mongodb_testing_{slug}"
    - key: ELASTICSEARCH_PREFIX
      template: "search_testing_{slug}"
    - key: REDIS_QUEUE_DATABASE
      template: "{slug_redis_queue}"
    - key: REDIS_CACHE_DATABASE
      template: "{slug_redis_cache}"

databases:
  - engine: mysql
    name_template: "myapp_testing_{slug}"
    dump:
      path: "tests/_data/dump.sql"
    migrations:
      framework: laravel
      dir: "database/migrations"
    # `clones: auto` consults the detected test framework: per-worker
    # frameworks (paratest, pytest-xdist, jest, …) get num_cpus clones;
    # shared-DB frameworks (plain pytest, mocha, go test, …) get one.
    # Pin an explicit number with `clones: 8` to override detection.
    paratest:
      clones: auto
      name_template: "myapp_testing_{slug}_test_{n}"
  - engine: mongodb
    name_template: "mongodb_testing_{slug}"
  - engine: redis
    namespaces:
      db_index_template: "{slug_redis_queue}"
  - engine: elasticsearch
    namespaces:
      index_prefix_template: "search_testing_{slug}"

hooks:
  postcreate:
    - run: "composer install --no-interaction"
    - run: "yarn install --frozen-lockfile"
      background: true
  predelete: []

watcher:
  paths:
    - glob: "database/migrations/**"
      on: auto
  debounce_ms: 500

# Optional: declare a custom migration framework
frameworks:
  app_mongo:
    markers:
      - "database/mongo_migrations/.marker"
    migration_dirs:
      - "database/mongo_migrations"
    file_pattern: "*.php"
    hash_mode: filename       # | checksum
    on_modify: rebuild        # | delta
    engine_hint: mongodb
```

### Engine catalog

The Configuration block above shows the common cases (MySQL, Postgres,
MongoDB, Redis, Elasticsearch). Below is a compact `connections.*` +
matching `databases:` entry for every supported engine. All blocks are
optional — declare only what you use.

Three template shapes recur:

| Shape | Used by | Field |
|---|---|---|
| `name_template` | Engines with a first-class "database" concept (MySQL, Postgres, etc.) | Per-worktree DB name |
| `namespaces.db_index_template` | Redis only | Numeric DB index 0..15 |
| `namespaces.prefix_template` / `namespaces.index_prefix_template` | Engines that key on string prefixes (Elasticsearch, Qdrant, S3, …) | Per-worktree prefix |

#### Relational

```yaml
connections:
  mysql:
    host: 127.0.0.1
    port: 3306
    user: root
    password_env: MYSQL_PWD
    pool_max: 8
  postgres:
    host: 127.0.0.1
    port: 5432
    user: postgres
    password_env: PGPASSWORD
    pool_max: 8

databases:
  # mysql variant also drives: mariadb, tidb
  - engine: mysql
    name_template: "myapp_{slug}"
    dump:
      path: "tests/_data/dump.sql"
    migrations:
      framework: laravel
      dir: "database/migrations"
    paratest:
      clones: auto
      name_template: "myapp_{slug}_test_{n}"
  # postgres variant also drives: cockroach
  - engine: postgres
    name_template: "myapp_{slug}"
    migrations:
      framework: sqlx
      dir: "migrations"
    paratest:
      clones: auto
      name_template: "myapp_{slug}_test_{n}"
```

#### Document / KV

```yaml
connections:
  mongodb:
    uri: mongodb://localhost:27017
  redis:
    url: redis://localhost:6379
  memcached:
    host: 127.0.0.1
    port: 11211
  etcd:
    url: http://localhost:2379

databases:
  - engine: mongodb
    name_template: "myapp_{slug}"
  - engine: redis
    namespaces:
      db_index_template: "{slug_redis_queue}"
  # memcached is namespaceless; name_template is informational
  - engine: memcached
    name_template: "myapp_{slug}"
  - engine: etcd
    namespaces:
      prefix_template: "/myapp/{slug}/"
```

#### Search

```yaml
connections:
  elasticsearch:
    url: http://localhost:9200
  meilisearch:
    url: http://localhost:7700
    api_key_env: MEILI_KEY
  typesense:
    url: http://localhost:8108
    api_key_env: TYPESENSE_KEY

databases:
  # elasticsearch variant also drives: opensearch
  - engine: elasticsearch
    namespaces:
      index_prefix_template: "search_{slug}_"
  # meilisearch variant also drives: typesense
  - engine: meilisearch
    namespaces:
      prefix_template: "{slug}_"
```

#### Vector

```yaml
connections:
  qdrant:
    url: http://localhost:6333
    api_key_env: QDRANT_KEY
  weaviate:
    url: http://localhost:8080
  milvus:
    url: http://localhost:19530

databases:
  # qdrant variant also drives: weaviate, milvus
  - engine: qdrant
    namespaces:
      prefix_template: "{slug}_"
```

#### Analytics

```yaml
connections:
  clickhouse:
    url: http://localhost:8123
    user: default
    password_env: CLICKHOUSE_PWD
  influxdb:
    url: http://localhost:8086
    token_env: INFLUX_TOKEN
    org_id: my-org

databases:
  - engine: clickhouse
    name_template: "myapp_{slug}"
  - engine: influxdb
    name_template: "myapp_{slug}"
```

#### Graph

```yaml
connections:
  neo4j:
    url: bolt://localhost:7687
    user: neo4j
    password_env: NEO4J_PWD

databases:
  - engine: neo4j
    name_template: "myapp_{slug}"
```

#### Queues

```yaml
connections:
  rabbitmq:
    url: http://localhost:15672
    user: guest
    password_env: RABBITMQ_PWD
  nats:
    url: http://localhost:8222
  kafka:
    host: 127.0.0.1
    port: 9092

databases:
  # rabbitmq name_template maps to a RabbitMQ vhost
  - engine: rabbitmq
    name_template: "myapp_{slug}"
  - engine: nats
    namespaces:
      prefix_template: "{slug}_"
  - engine: kafka
    namespaces:
      prefix_template: "{slug}_"
```

#### Object store

```yaml
connections:
  s3:
    endpoint: http://localhost:9000
    region: us-east-1
    access_key_env: MINIO_ACCESS
    secret_key_env: MINIO_SECRET

databases:
  # bucket names disallow `_`, so use {slug_dash}
  - engine: s3
    namespaces:
      prefix_template: "{slug_dash}-"
```

#### Embedded files

```yaml
connections:
  duckdb:
    base_dir: ~/.local/state/treeman/duckdb

databases:
  - engine: sqlite
    name_template: "tests/_data/test_{slug}.sqlite"
  - engine: duckdb
    name_template: "{slug}.duckdb"
```

### Slug derivation

- If the branch (or worktree basename) matches `[A-Z]+-\d+`, slug =
  `<prefix>_<num>` lowercased (`PROJ-123` → `proj_123`). Stable across
  renames.
- Else slug = `wt_<blake3(canonical-path)[..8]>`. Stable across runs.

Derived template vars: `{slug}`, `{slug_dash}` (underscores → hyphens for
S3/minio buckets), `{slug_redis_queue}` and `{slug_redis_cache}`
(deterministic indices 6..15), `{n}` (1-indexed paratest clone).

---

## Migration framework matrix

| Framework | Markers | Migration dirs (single-app + monorepo) | Hash mode | On modify |
|---|---|---|---|---|
| Laravel | `artisan` | `database/migrations`, `app/Modules/*/Database/Migrations`, `Modules/*/Database/Migrations`, lowercase variants | filename | rebuild |
| Rails | `bin/rails`, `Gemfile`, `config/database.yml` | `db/migrate`, `engines/*/db/migrate` | filename | rebuild |
| Django | `manage.py` | `**/migrations` | filename | rebuild |
| golang-migrate | `go.mod` | `**/migrations`, `services/*/migrations`, `cmd/*/migrations` | filename | rebuild |
| sqlx-cli | `Cargo.toml`, `migrations` | `migrations`, `crates/*/migrations`, `services/*/migrations` | checksum | delta |
| Diesel | `diesel.toml` | `migrations`, `crates/*/migrations` | filename | rebuild |
| Prisma | `prisma/schema.prisma` | `prisma/migrations`, `apps/*/prisma/migrations`, `packages/*/prisma/migrations` | checksum | delta |
| Knex | `knexfile.js` | `migrations`, `apps/*/migrations`, `packages/*/migrations` | filename | rebuild |
| Alembic | `alembic.ini` | `**/versions` | filename | rebuild |
| Flyway | `flyway.conf` | `**/db/migration` | checksum | rebuild |
| TypeORM | `package.json` | `src/migrations`, `apps/*/src/migrations`, `packages/*/src/migrations` | filename | rebuild |
| Drizzle | `drizzle.config.ts` | `drizzle`, `apps/*/drizzle`, `packages/*/drizzle` | checksum | delta |
| Sequelize | `.sequelizerc` | `migrations`, `apps/*/migrations`, `packages/*/migrations` | filename | rebuild |
| MikroORM | `mikro-orm.config.ts` | `src/migrations`, `apps/*/src/migrations`, `packages/*/src/migrations` | filename | rebuild |

Hash mode + on-modify control watcher dispatch (see plan §7):

- **Filename + rebuild** — frameworks that track migration names (not
  content) in a `migrations` table. Any edit to an existing migration is
  silently ignored by the framework, so treeman must wipe + reseed.
- **Checksum + delta** — frameworks like sqlx-cli, Prisma, Drizzle that
  record checksums. Renames of unmodified files are no-ops; edits force
  rebuild; new files apply as delta.

### Adding your own framework

Drop a `frameworks:` block into `.treeman.yaml`. Same-name override
replaces the built-in. No recompile.

---

## Test framework matrix

`treeman` detects the test runner used by your repo and uses it to pick
the replication strategy (one DB clone per worker, or a single shared
DB). Detection runs on every `prepare` / watcher rebuild; no config
needed.

| Test framework | Marker | Strategy | Worker index | Worker env |
|---|---|---|---|---|
| paratest (PHP) | `composer.json` has `brianium/paratest` | per-worker | 1-based | `TEST_TOKEN` |
| pest (PHP) | `composer.json` has `pestphp/pest` | per-worker | 1-based | `TEST_TOKEN` |
| phpunit (PHP) | `composer.json` has `phpunit/phpunit` (no paratest) | shared | — | — |
| pytest-xdist | `pytest-xdist` in Python deps | per-worker | `gw0`, `gw1`, … | `PYTEST_XDIST_WORKER` |
| pytest-parallel | `pytest-parallel` in Python deps | per-worker | `gw0`, `gw1`, … | `PYTEST_XDIST_WORKER` |
| pytest (plain) | `pytest` in Python deps (no xdist) | shared | — | — |
| parallel_tests (Ruby) | `Gemfile` has `parallel_tests` | per-worker | 1-based | `TEST_ENV_NUMBER` |
| rspec / minitest | `Gemfile` has `rspec`/`minitest` (no parallel_tests) | shared | — | — |
| jest | `package.json` has `jest` | per-worker | 1-based | `JEST_WORKER_ID` |
| vitest | `package.json` has `vitest` | per-worker | 1-based | `VITEST_POOL_ID` |
| playwright | `package.json` has `@playwright/test` | per-worker | 0-based | `TEST_PARALLEL_INDEX` |
| mocha-parallel-tests | `package.json` has `mocha-parallel-tests` | per-worker | 1-based | `MOCHA_PARALLEL_INDEX` |
| mocha (plain) | `package.json` has `mocha` (no parallel) | shared | — | — |
| cargo-nextest | `.config/nextest.toml` or `nextest` in `Cargo.toml` | per-worker | 0-based | `NEXTEST_TEST_GLOBAL_SLOT` |
| cargo test | `Cargo.toml` (no nextest) | shared | — | — |
| go test | `go.mod` | shared | — | — |
| Maven Surefire | `pom.xml` | per-worker | 1-based | `SUREFIRE_FORK_NUMBER` |
| Gradle | `build.gradle{,.kts}` | per-worker | 1-based | `GRADLE_TEST_WORKER` |
| dotnet test | `*.csproj`/`*.fsproj`/`*.sln` | shared | — | — |

Run `treeman fw detect` to see what was matched in the current repo,
including the suggested clone count.

---

## Daemon lifecycle

`treeman daemon` manages the `treemand` process across Linux and macOS:

| OS | Service backend | Install location |
|---|---|---|
| Linux | systemd `--user` | `~/.config/systemd/user/treemand.service` |
| macOS | launchd LaunchAgent | `~/Library/LaunchAgents/com.treeman.daemon.plist` |

```sh
treeman daemon install      # writes the unit/plist and starts it
treeman daemon start        # systemctl/launchctl if installed, else spawn detached
treeman daemon stop         # service-aware
treeman daemon restart      # stop + start
treeman daemon uninstall    # stop, remove unit/plist, reload
treeman daemon status       # version, pid, started_at, watcher count
```

`start`/`stop` are service-aware: when the user-service unit/plist
exists, `start` invokes `systemctl --user start` (or
`launchctl bootstrap`); when not installed, it spawns `treemand`
detached with `setsid` (Linux) / `nohup` (macOS). `uninstall` is
idempotent — safe to call when nothing is installed.

---

## Watcher behavior

### Per-worktree fan-out

`treemand` runs one *worktree-index* watcher per registered repo that
observes `.git/worktrees/` via `notify` (no polling, no `git`
subprocess). On every diff:

- **Added worktree** → spawn a framework-watcher set scoped to that
  worktree path, with its own slug and its own per-worktree config layer
  (`<wt>/.treeman.local.yaml` is overlaid on top of the main repo's
  config).
- **Removed worktree** → abort that worktree's tasks.

Triggered by anything that touches `.git/worktrees/`: `treeman wt
create`/`delete`, plain `git worktree add`/`remove`/`prune`, IDE
worktree commands. The CLI never needs to notify the daemon.

A nesting guard rejects worktree paths that fall inside `.git/` or
inside any already-tracked worktree. `treeman wt delete` cleans up
empty parent directories up to `worktrees.root` so the layout stays
tidy.

### Per-framework loop

Each detected framework runs in its own task. On every debounced event:

1. Dynamically expand watch coverage. If a parent dir for a glob pattern
   (e.g. `app/Modules/`, `engines/`, `apps/`) was just created, the
   recursive watcher picks it up via re-resolve.
2. Recompute hash inputs over the union of all matching dirs.
3. Compare against the on-disk state in
   `<repo>/.treeman/watch-state-<framework>.json`.
4. Dispatch:

| Event | Filename mode | Checksum mode |
|---|---|---|
| New file | `Delta(new_keys)` | `Delta(new_keys)` |
| Content changed (same name) | `Rebuild` | `Rebuild` |
| Rename only (same content) | `Rebuild` | `Noop` |
| File deleted | `Rebuild` | `Rebuild` |
| Lockfile changed | `Rebuild` | `Rebuild` |

5. `Delta` and `Rebuild` both currently invoke `treeman_prepare::run`
   (full prepare with cache-hit fast path). MySQL binlog-based delta
   replay is M9 — scaffolded but the row-image → DML rewrite isn't yet
   wired.

Heavy directories are pruned from walks (`.git`, `node_modules`,
`.pnpm-store`, `.yarn`, `vendor`, `target`, `build`, `dist`, `.venv`,
`__pycache__`, `.gradle`, `.m2`, `.idea`, `.vscode`, `.next`, `.nuxt`,
`tmp`, etc.).

### Boot-time resume

On startup, `treemand` reads every registered repo from SQLite and
re-spawns its watcher set. Missing-`.git` repos log a warning and the
rest continue. Lets `systemctl restart treemand` (or a host reboot)
regain full coverage with zero CLI intervention.

### Config hot-reload

Changes to `<repo>/.treeman.yaml`, `<repo>/.treeman.local.yaml`,
`<wt>/.treeman.local.yaml`, or `~/.config/treeman/config.yaml` trigger
a clean stop+start of that repo's watcher with the new config. Editor
backup files (`.swp`, `~`, …) are filtered out before the reload
fires.

### Snapshot GC

A background task in `treemand` periodically calls the snapshot
catalog GC (`treeman_snapshot::run_gc`). It evicts catalog rows per
the `snapshots.retention.*` policy and then DROPs the matching
template database in the engine (MySQL/MariaDB/TiDB, Postgres/Cockroach,
MongoDB, Elasticsearch/Opensearch). Engines without a snapshot strategy
log+skip. The interval is the smallest `gc_interval_minutes` across
registered repos, floor-clamped to 5 minutes.

---

## Architecture

```
┌──────────────┐  unix socket   ┌────────────────────────────────────┐
│   treeman    │ ─────────────► │             treemand               │
│   (CLI)      │ JSON RPC       │ ┌────────────────────────────────┐ │
└──────────────┘                │ │ shared sqlx pools              │ │
                                │ │ (mysql, pg, mongo, redis, …)   │ │
                                │ └────────────────────────────────┘ │
                                │ ┌────────────────────────────────┐ │
                                │ │ per-repo:                      │ │
                                │ │   .git/worktrees/ index task   │ │
                                │ │   config-file watcher          │ │
                                │ │     ↓ (worktree diff)          │ │
                                │ │   per-worktree framework set   │ │
                                │ │   (notify + debouncer-full)    │ │
                                │ └────────────────────────────────┘ │
                                │ ┌────────────────────────────────┐ │
                                │ │ periodic snapshot GC           │ │
                                │ └────────────────────────────────┘ │
                                │ ┌────────────────────────────────┐ │
                                │ │ tracing → SQLite events        │ │
                                │ └────────────────────────────────┘ │
                                └────────────────────────────────────┘
```

Crates (cargo workspace):

| Crate | Role |
|---|---|
| `treeman-proto` | IPC wire types (Request/Response enums) |
| `treeman-core` | Config, slug, env patcher, hook runner |
| `treeman-store` | SQLite schema, tracing layer, event queries |
| `treeman-db` | DbDriver trait + mysql/pg/mongo/redis/es impls + dumpload + binlog scaffold |
| `treeman-migrations` | Framework registry + 14 built-in detectors + migrate runner |
| `treeman-watcher` | notify-debouncer-full watchers: `.git/worktrees/` index, config files, per-framework migration dirs |
| `treeman-snapshot` | SnapshotKey fingerprinting + cache catalog + paratest fanout + periodic GC (`run_gc`) |
| `treeman-prepare` | The full prepare orchestrator |
| `treeman-daemon` | `treemand` binary |
| `treeman-cli` | `treeman` binary |

---

## Hacking

Requires:

- Rust 1.85+ (edition 2024). `rust-toolchain.toml` pins the channel.
- `just` for the project commands (optional but recommended).
- `nix` for the flake-based dev shell (optional).

```sh
just test         # cargo test --workspace
just lint         # cargo clippy --all-targets --all-features -- -D warnings
just fmt          # cargo fmt --all
just build        # debug build
just build-release
```

### Release

`just` recipes bump version, refresh locks, run `check`, commit, tag,
push, and push the tag — which triggers the GitHub Actions release
workflow.

```sh
just release-preview     # dry-run: show next version for each bump
just release-patch       # 0.0.1 → 0.0.2
just release-minor       # 0.0.2 → 0.1.0
just release-major       # 0.1.0 → 1.0.0
```

Each release recipe:

1. Runs `just check-tools` (cargo, git, gh, cargo-edit; optional nix).
2. Verifies the working tree is clean on the default branch (auto-
   detected via `git rev-parse --abbrev-ref origin/HEAD`; falls back
   to `main`).
3. Runs `just check` (= `fmt-check` + `clippy -D warnings` + `cargo
   test --workspace`). Auto-commits any formatting drift it produces.
4. `nix flake update` (if nix is installed); commits the lock change.
5. `nix build --no-link .#workspace` — verifies the package still
   builds under nix. Crane derives hashes from `Cargo.lock`, so no
   manual hash patching is needed (unlike Go's `vendorHash` dance).
6. `cargo set-version --workspace --bump {patch,minor,major}`.
7. `cargo update --workspace` to refresh `Cargo.lock`.
8. `cargo build --workspace --release` — final sanity build.
9. `git commit -m "release vX.Y.Z"`, `git tag -a vX.Y.Z -m vX.Y.Z`.
10. `git push origin HEAD && git push origin vX.Y.Z`.

The tag push fires `.github/workflows/release.yml` which builds the
four target tarballs and publishes them as the GitHub release.

### Nix dev shell

```sh
nix develop                  # drops into a shell with rustc/cargo/just/sqlx-cli
nix build .#treeman          # builds the CLI binary
nix build .#treemand         # builds the daemon binary
nix run .#treeman -- status
```

---

## Tested against real engines

`treeman-db` ships a live integration suite covering MySQL, PostgreSQL,
and Redis. Each test scopes itself to a unique `tm_it_<hash>_*`
database (or Redis DB index 1..15) and drops the scratch state on
completion. The backing infra is chosen per-run:

1. If `TREEMAN_TEST_MYSQL_URL` / `TREEMAN_TEST_POSTGRES_URL` /
   `TREEMAN_TEST_REDIS_URL` is set, tests use it directly. Lets you
   point the suite at your dev infra so iteration is fast.
2. If the env var is unset, the test spins up the relevant engine via
   `testcontainers` so CI can run the suite with no pre-provisioned
   databases. Treeman never spawns containers at runtime — this is
   strictly a test-pipeline concession.

```sh
# Option A — point at existing infra.
export TREEMAN_TEST_MYSQL_URL=mysql://root:@127.0.0.1:3306/
export TREEMAN_TEST_POSTGRES_URL=postgres://postgres:@127.0.0.1:5432/
export TREEMAN_TEST_REDIS_URL=redis://127.0.0.1:6379/

# Option B — let testcontainers spin one up (CI default).
unset TREEMAN_TEST_MYSQL_URL TREEMAN_TEST_POSTGRES_URL TREEMAN_TEST_REDIS_URL

cargo test --features integration -p treeman-db -- --include-ignored
```

Tests are gated by `--features integration` and marked `#[ignore]` so
the default `cargo test` invocation runs only the pure-logic unit
suite.

## Delta migrate path

When the watcher detects newly-added migration files (`Dispatch::Delta`),
`treeman-prepare::delta_run` invokes the framework's pending-migration
command (e.g. `php artisan migrate --force` for Laravel,
`sqlx migrate run` for sqlx-cli) against the source DB and every
paratest clone. Frameworks track applied migrations themselves, so the
pending-only invocation is idempotent and skips the dump-reload + full
re-clone that `Rebuild` requires.

The `treeman-db::binlog` module ships a `BinlogReplicator` skeleton for
MySQL binlog row-event replay — useful if the migration includes seed
data the framework can't reproduce. Wiring real DML application against
arbitrary table schemas is non-trivial and tracked as a future
optimization; the framework-migrate delta above covers the common case
correctly and is the default.

---

## License

MIT OR Apache-2.0.
