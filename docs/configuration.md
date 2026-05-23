# Configuration

[← back to README](../README.md)

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
    migrate:                               # shell command + env-var redirects (point the CLI at the per-run template DB)
      run: "php artisan migrate --force"
      env:
        DB_DATABASE: "{target_db}"
        DB_TEST_DATABASE: "{target_db}"
    inputs:                                # files folded into the snapshot key; also watched for changes
      - { glob: "database/migrations/**/*.php", label: migrations, hash: filename }
      - composer.lock                      # bare string = checksum hash, no label
    test_clones:                           # parallel-test-runner fan-out
      clones: auto                         # auto = detect from phpunit.xml / pyproject / Jest config
      name_template: "myapp_testing_{slug}_test_{n}"

hooks:
  on-create-before-engines:                # sequenced, runs before engines come up
    - "git pull --ff-only"
  on-create-after-engines:                 # async (parallel groups)
    - composer install --no-interaction --prefer-dist
    - yarn install --frozen-lockfile
    - group:
        - cd frontend && yarn install
        - cd frontend && yarn build:dev
  on-delete-before-engines:                # async; runs before git worktree remove
    - "echo dropping caches"
  on-file-change:                          # filtered by `match:` against input labels
    - match: migrations
      run: "echo migrations changed"

snapshots:
  cache_dir: ~/.cache/treeman/snapshots    # only used by GC reports
  retention:
    cap_per_repo: 8                        # NEW: hard cap, LRU evicts on new generation
    max_age_days: 30
    max_total_gb: 50
    gc_interval_minutes: 60                # daemon background sweep

watcher:
  debounce_ms: 500
  binlog:
    enabled: true                          # MySQL only — see Binlog section in advanced.md

daemon:
  socket: $XDG_RUNTIME_DIR/treeman.sock
  log_level: info
```

## Templated names

Several config fields are template strings rendered with the
worktree's slug:

| Token | Example |
|---|---|
| `{slug}` | `proj_123` |
| `{slug_dash}` | `proj-123` |
| `{slug_upper}` | `PROJ_123` |
| `{slug_redis_index}` | `7` (deterministic 0–15 hash of slug) |
| `{n}` | test-clone index (1-based) |

## Fully declarative — no hidden defaults

treeman runs against what `.treeman.yaml` says, not what it
guesses from filesystem markers. Init helpers (`treeman init`,
`treeman fw detect`) inspect the repo and write the corresponding
YAML; once written, those fields are authoritative. If a migration
glob lives somewhere unusual, change `inputs:` and the next prepare
sees it — no recompile, no rebuild of treeman, no per-framework
code path. The same applies to env files: only what
`env_scoping.sources` lists is read.

The built-in framework presets exist solely as init-time templates
and are listed by `treeman fw detect`. Copy fields in by hand for
custom layouts.

## Hook groups

Each entry under `on-create-after-engines` / `on-delete-before-engines`
/ `on-delete-after-engines` is a **group**. Within a group: commands
run in sequence (first non-zero exit aborts the group). Across groups:
groups run in **parallel**. Each group becomes one `setsid`-detached
driver, so the CLI returns immediately after spawning drivers.

Three forms:

```yaml
hooks:
  on-create-after-engines:
    # bare string — one-command group
    - "composer install"

    # map — one-command group with extra fields
    - { run: "yarn build", cwd: frontend, env: { NODE_ENV: production } }

    # sequence — multi-command group, commands chain with &&
    - group:
        - "npm install"
        - "npm run build"
```

Every hook trigger is async-dispatched — the CLI returns immediately
after spawning drivers. The available triggers are:
`on-create-before-engines`, `on-create-after-engines`,
`on-delete-before-engines`, `on-delete-after-engines`,
`on-checkout`, and `on-file-change`. The `*-before-engines` variants
run before treeman touches its managed engines, the `*-after-engines`
variants after.

### Running hooks inside a container

Hook entries accept an `in_container:` (or `compose_service:`)
directive that wraps every step in `<engine> exec` so the command
runs inside the named container rather than on the host. Useful for
`composer install`, `npm ci`, `php artisan migrate`,
`bundle install`, … that depend on the dev container's toolchain.

```yaml
hooks:
  on-create-after-engines:
    # Single-step group, in a named container.
    - { run: "composer install", in_container: myapp-php }

    # Multi-step group, in a compose service.
    - compose_service: app
      compose_project: myapp
      run:
        - "npm ci"
        - "php artisan migrate"
```

`step.cwd` becomes `-w <cwd>` on the `exec` call (interpreted
inside the container's filesystem). Leave unset to use the
container's `WORKDIR`.

## Example: per-stack `databases:` blocks

`treeman init` writes these for you when it detects the matching
framework. Copy + paste into the `databases:` array of an existing
`.treeman.yaml` to add a stack.

**Rails + Postgres** (db/migrate, Gemfile.lock invalidates):

```yaml
- engine: postgres
  name_template: "myapp_test_{slug}"
  migrate:
    run: "bin/rails db:migrate"
    env: { DATABASE: "{target_db}" }
  inputs:
    - { glob: "db/migrate/**/*.rb", label: migrations, hash: filename }
    - Gemfile.lock
  test_clones:
    clones: auto          # reads parallel_workers from config/test.rb / spec_helper
    name_template: "myapp_test_{slug}_w{n}"
```

**Django + Postgres** (auto-discovered app `migrations/` dirs):

```yaml
- engine: postgres
  name_template: "myapp_test_{slug}"
  migrate:
    run: "python manage.py migrate --noinput"
    env: { DJANGO_DB_NAME: "{target_db}" }
  inputs:
    - { glob: "**/migrations/[0-9]*_*.py", label: migrations, hash: filename }
    - poetry.lock
    - Pipfile.lock
    - requirements.txt
  test_clones:
    clones: auto          # reads pytest -n / pytest-xdist config
    name_template: "myapp_test_{slug}_w{n}"
```

**golang-migrate + MySQL**:

```yaml
- engine: mysql
  name_template: "svc_test_{slug}"
  migrate:
    run: "migrate up"
    env: { MIGRATE_DATABASE_NAME: "{target_db}" }
  inputs:
    - { glob: "migrations/**/*.up.sql", label: migrations, hash: filename }
    - { glob: "services/*/migrations/**/*.up.sql", label: migrations, hash: filename }
    - go.sum
  test_clones:
    clones: 4             # explicit count; Go's `-parallel` is per-package
    name_template: "svc_test_{slug}_w{n}"
```

**sqlx-cli + Postgres** (migrations are mutable — checksum hash via default):

```yaml
- engine: postgres
  name_template: "app_test_{slug}"
  migrate:
    run: "sqlx migrate run"
    env: { DATABASE_URL_NAME: "{target_db}" }
  inputs:
    # bare-string default = checksum hash, so edits to a migration
    # rebuild the snapshot (sqlx allows mutable migrations).
    - "migrations/**/*.sql"
    - "crates/*/migrations/**/*.sql"
    - Cargo.lock
  test_clones:
    clones: auto          # reads cargo nextest config
    name_template: "app_test_{slug}_w{n}"
```

## Credential resolution from .env

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

## Connecting to DBs in a container

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
