# Configuration

[← back to README](../README.md)

treeman reads config from two files that merge into one effective
config, last-write-wins:

1. `~/.config/treeman/config.yaml` — **user-global**, machine-wide defaults shared by every repo. Scaffold with `treeman init --global`.
2. `<repo>/.treeman.yaml` — **per-repo**, committed. Plus an optional `.treeman.local.yaml` overlay (gitignored).

Each top-level key has a **scope** that decides which file it may live
in. **This is a hard rule — a key in the wrong file is an error at load
time, with no flag to relax it** (the layered merge made a misplaced key
silently inert, so it's surfaced loudly instead). Editors enforce it too:
`.treeman.yaml` validates against the repo-scoped schema and the global
config against the global-scoped one (`treeman schema install` /
`treeman init --global` wire the right modeline).

| Scope | Keys | Lives in |
|-------|------|----------|
| **global** | `daemon`, `snapshots`, `logs`, `status`, `notifications` | `~/.config/treeman/config.yaml` only |
| **repo** | `databases`, `patches`, `hooks`, `main_worktree`, `env_sources` | `.treeman.yaml` only |
| **both** | `connections`, `worktrees`, `auto_fetch`, `ports`, `frameworks`, `debounce_ms` | either (global = default, repo overrides) |

For the full per-key reference + auto-generated examples see
[config-reference.md](config-reference.md).

### Per-repo `.treeman.yaml`

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/stubbedev/treeman/master/schemas/treeman.schema.json

worktrees:
  root: .worktrees                # default
  copies: [".env"]                # copied into each worktree (patched per-branch; never shared)
  links: ["node_modules"]         # symlinked from main (shared read-only cache)

env_sources:                       # credential-resolver READ list
  - .env
  - .env.testing

# The whole `connections:` block is optional — treeman auto-resolves
# credentials from `.env` / `.env.testing` (Laravel `DB_*`,
# Spring-Boot `SPRING_DATASOURCE_*`, MySQL/PG/Redis component vars).
# Declare anything below only when you want to override the auto-
# detected value. `connections` is scope "both": set machine-wide
# defaults in the global config, override per-repo here.
connections:
  mysql:
    host: 127.0.0.1
    port: 3306
    user: root
    password: $MYSQL_ROOT_PASSWORD     # overrides .env DB_PASSWORD
  postgres:
    host: 127.0.0.1
    port: 5432
    user: postgres
    password: $PGPASSWORD
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
      - { glob: "database/migrations/**/*.php", label: migrations }
      - composer.lock                      # bare string = glob with no label
    test_clones:                           # parallel-test-runner fan-out
      clones: auto                         # auto = one clone per CPU when the detected framework parallelizes per-worker
      name_template: "myapp_testing_{slug}_test_{n}"

hooks:
  # Every entry under a trigger key is an action; actions in one
  # list run in PARALLEL. `run:` itself takes a string (one step) or
  # a list (steps chain sequentially with `&&` — failure short-circuits).
  create-before-engines:                # fires after patches + bring-in, BEFORE engine prepare
    - run: "git pull --ff-only"
  create-after-engines:                 # fires after engine prepare
    - run: composer install --no-interaction --prefer-dist
    - run: yarn install --frozen-lockfile
    - cwd: frontend                        # multi-step action — cd + build chain sequentially
      run:
        - yarn install
        - yarn build:dev
  delete-before-engines:                # fires BEFORE engine drop + git remove
    - run: "echo dropping caches"
  file-change:                          # filtered by `match:` against input labels
    - match: migrations                    # accepts string or list (e.g. [migrations, seeders])
      run: "echo migrations changed"

debounce_ms: 500                         # file-watcher coalesce window (scope "both")
```

### User-global `~/.config/treeman/config.yaml`

Machine-wide settings. `daemon`, `snapshots`, `logs`, `status`, and
`notifications` are **only** valid here — putting them in a repo
`.treeman.yaml` is a hard error.

```yaml
daemon:
  socket: $XDG_RUNTIME_DIR/treeman.sock
  log_level: info                        # debug | info | warn | error

snapshots:
  cache_dir: ~/.cache/treeman/snapshots    # only used by GC reports
  retention:
    cap_per_repo: 8                        # hard cap, LRU evicts on new generation
    max_age_days: 30
    max_total_gb: 50
    gc_interval_minutes: 60                # daemon background sweep

logs:
  keep_days: 14                            # daemon prunes the shared event log; 0 keeps forever

auto_fetch:                                # scope "both" — global default cadence; a repo may opt out
  enabled: true
  interval_minutes: 15

notifications:
  enabled: false                           # desktop banners on worktree lifecycle changes
```

## Templated names

Several config fields are template strings rendered with the
worktree's slug:

| Token | Example |
|---|---|
| `{slug}` | `proj_123` |
| `{slug_dash}` | `proj-123` |
| `{slug_redis_queue}` | `9` — checksum-derived Redis DB index (6–15) |
| `{slug_redis_cache}` | `12` — checksum-derived Redis DB index (6–15), distinct from queue |
| `{port_<name>}` | allocated port for the `ports:` slot `<name>` (e.g. `{port_octane}`) |
| `{target_db}` | the rendered per-run DB name (only in `migrate.env` / `seed.env`) |
| `{n}` | test-clone index, 1-based (only in `test_clones.name_template`) |

## Fully declarative — no hidden defaults

treeman runs against what `.treeman.yaml` says, not what it
guesses from filesystem markers. Init helpers (`treeman init`,
`treeman fw detect`) inspect the repo and write the corresponding
YAML; once written, those fields are authoritative. If a migration
glob lives somewhere unusual, change `inputs:` and the next prepare
sees it — no recompile, no rebuild of treeman, no per-framework
code path. The same applies to env files: only what
`env_sources:` lists is read.

The built-in framework presets exist solely as init-time templates
and are listed by `treeman fw detect`. Copy fields in by hand for
custom layouts.

## Hooks

Each entry under a trigger key (e.g. `create-after-engines`) is
one **action**. Actions in the same list run in **parallel**. The
`run:` field inside one action is the action's step list — steps
chain sequentially with `&&` so the first non-zero exit aborts the
remaining steps of that action.

Every action must be a mapping with at minimum a `run:` field
(bare-string entries like `- "composer install"` were removed in
2.x — wrap them as `- run: "composer install"`).

```yaml
hooks:
  create-after-engines:
    # single-step action — `run:` is a string
    - run: "composer install"

    # action that pins cwd (host-side) and runs ONE step
    - cwd: frontend
      run: "yarn build"

    # multi-step action — steps chain with &&, abort on first failure
    - cwd: frontend
      run:
        - "yarn install"
        - "yarn build:dev"
```

Available triggers: `create-before-engines`,
`create-after-engines`, `delete-before-engines`,
`delete-after-engines`, `checkout`, `file-change`. The
`*-before-engines` variants run before treeman touches its managed
engines, the `*-after-engines` variants after. Every hook trigger is
async-dispatched — the CLI returns immediately after spawning
drivers.

Per-step env vars are only available on `databases[].migrate.env`
and `databases[].seed.env` (the framework-runner step), not on
hook actions. Use the inherited shell environment + `cwd:` to
control hook subprocesses.

### Running hooks inside a container

Hook entries accept a `container:` (or `compose_service:`) directive
that wraps every step in `<engine> exec` so the command runs inside
the named container rather than on the host. Useful for
`composer install`, `npm ci`, `php artisan migrate`,
`bundle install`, … that depend on the dev container's toolchain.

```yaml
hooks:
  create-after-engines:
    # Single-step action, in a named container.
    - { run: "composer install", container: myapp-php }

    # Multi-step action, in a compose service.
    - compose_service: app
      compose_project: myapp
      run:
        - "npm ci"
        - "php artisan migrate"
```

`action.cwd` becomes `-w <cwd>` on the `exec` call (interpreted
inside the container's filesystem). Leave unset to use the
container's `WORKDIR`.

### Hook environment variables

Every hook subprocess inherits the user's `wt create`-time env (PATH,
shims, `.env`-loaded vars) plus four scoping variables treeman sets
unconditionally:

| Variable           | Value                                                                  |
| ------------------ | ---------------------------------------------------------------------- |
| `TREEMAN_MAIN_ROOT`| Absolute path of the repo root (`git rev-parse --show-toplevel`).      |
| `TREEMAN_WORKTREE` | Absolute path of the worktree firing the hook.                         |
| `TREEMAN_SLUG`     | The slug used to name resources for this run (e.g. `feature_x`, `main_develop`). |
| `TREEMAN_IS_MAIN`  | `"1"` when the firing worktree is the repo root with main-wt enabled, else `"0"`. Branch on this to skip dev-only setup on linked worktrees, or vice versa. |

`file-change` hooks additionally receive `TREEMAN_WATCH_PATH`,
`TREEMAN_WATCH_LABEL`, `TREEMAN_WATCH_ENGINE`, `TREEMAN_WATCH_DB_NAME`
so the script can branch on the trigger details.

```yaml
hooks:
  create-after-engines:
    - run: |
        if [ "$TREEMAN_IS_MAIN" = "1" ]; then
          composer install --no-dev
        else
          composer install
        fi
```

## Main worktree

By default, treeman only manages **linked** worktrees under
`worktrees.root`. The repo root checkout is invisible to the
watcher — branch switches there don't trigger prepare, and the
checkout doesn't get its own per-branch databases.

`main_worktree:` opts the repo root into the same lifecycle. Once
enabled, flipping a branch at the repo root produces per-branch
databases keyed by `main_<branch>` (instead of every non-ticket
branch collapsing to one path-hash slug), and the checkout /
file-change / create-* hooks fire against the repo root the
same way they do for linked worktrees.

```yaml
main_worktree:
  enabled: true
  # Optional: per-context overlay for the databases[] block.
  # Sparse, index-aligned with databases[]. Set fields replace the
  # base entry's field; unset fields inherit.
  databases:
    - name_template: "app_dev_{slug}"       # different DB name in main wt
      test_clones:
        clones: 0                            # no paratest fanout in main
        name_template: ""
```

Flip on with `treeman main enable` — this patches `.treeman.yaml`,
asks the daemon to reload, then dispatches a finalize against the
repo root so `create-*` hooks run immediately. Reverse with
`treeman main disable` (config flag flips, watcher stops, databases
remain). Add `--purge` to drop every per-branch DB the
`main_<branch>` slug owns across all local branches.

`treeman main status` reports the current enrollment state +
branch-aware slug.

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
    env: { DB_NAME: "{target_db}" }
  inputs:
    - { glob: "db/migrate/**/*.rb", label: migrations }
    - Gemfile.lock
  test_clones:
    clones: auto          # auto = one clone per CPU (framework detected, not worker-config parsed)
    name_template: "myapp_test_{slug}_w{n}"
```

**Django + Postgres** (auto-discovered app `migrations/` dirs):

```yaml
- engine: postgres
  name_template: "myapp_test_{slug}"
  migrate:
    run: "python manage.py migrate --noinput"
    env: { DB_NAME: "{target_db}" }
  inputs:
    - { glob: "**/migrations/[0-9]*_*.py", label: migrations }
    - poetry.lock
    - Pipfile.lock
    - requirements.txt
  test_clones:
    clones: auto          # auto = one clone per CPU (framework detected, not worker-config parsed)
    name_template: "myapp_test_{slug}_w{n}"
```

**golang-migrate + Postgres**:

```yaml
- engine: postgres
  name_template: "svc_test_{slug}"
  migrate:
    # The CLI needs -path + -database; pull the DSN from
    # DATABASE_URL so treeman's per-run substitution can swap the
    # database portion.
    run: 'migrate -path migrations -database "$DATABASE_URL" up'
    env:
      DATABASE_URL: "postgres://user:password@127.0.0.1:5432/{target_db}?sslmode=disable"
  inputs:
    - { glob: "migrations/**/*.up.sql", label: migrations }
    - { glob: "services/*/migrations/**/*.up.sql", label: migrations }
    - go.sum
  test_clones:
    clones: 4             # explicit count; Go's `-parallel` is per-package
    name_template: "svc_test_{slug}_w{n}"
```

**sqlx-cli + Postgres** (sqlx allows in-place migration edits — content hashing catches them):

```yaml
- engine: postgres
  name_template: "app_test_{slug}"
  migrate:
    run: "sqlx migrate run"
    env:
      # sqlx-cli reads DATABASE_URL natively.
      DATABASE_URL: "postgres://user:password@127.0.0.1:5432/{target_db}?sslmode=disable"
  inputs:
    # every matched file is content-hashed, so an in-place edit
    # moves the fingerprint and rebuilds the snapshot.
    - "migrations/**/*.sql"
    - "crates/*/migrations/**/*.sql"
    - Cargo.lock
  test_clones:
    clones: auto          # auto = one clone per CPU (framework detected, not worker-config parsed)
    name_template: "app_test_{slug}_w{n}"
```

## Credential resolution from .env

treeman reads env files **only when** `env_sources:` is declared.
There is no implicit default — the runtime never reads files you
didn't list. `treeman init` writes an `env_sources:` block matching
the framework it detected (Laravel → `.env.testing` etc.); edit the
list to suit.

```yaml
env_sources:                # files read in order, last wins
  - .env
  - .env.testing        # baseline for tests
  - .env.testing.local  # per-dev overrides
```

Relative paths resolve against the repo root; absolute paths are
honoured as-is so you can pull from outside the repo (e.g. a
shared secret store).

When `env_sources:` resolves to a non-empty environment, the
resolver fills any unset fields in the `connections:` block.
Supported flavours per engine:

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
    container_engine: docker      # docker | podman | nerdctl | finch | …
    container_network: myapp_net  # optional: pin which network's IP to use
    port: 3306                    # internal port; defaults to engine default
    user: root
    password: $MYSQL_ROOT_PASSWORD   # or omit — see "Container env" below

  postgres:
    compose_service: db           # docker-compose service name (alternative to container:)
    compose_project: myapp        # defaults to $COMPOSE_PROJECT_NAME if unset
    user: postgres

  mongodb:
    container: myapp-mongo
    uri: "mongodb://placeholder:27017"   # host:port rewritten at dial time
```

**Container env autodiscovery.** When `password:` is unset and
the resolver still has no password from env files, treeman pulls
`MYSQL_ROOT_PASSWORD` / `POSTGRES_PASSWORD` straight out of the
container's `Config.Env`. Skip the password block when you've
already declared the secret on the container itself.

**Engines.** Anything that supports `inspect` and `ps --filter
label=...` works — `docker`, `podman`, `nerdctl`, `finch`. Default
is `docker`. OrbStack users keep the default — its CLI is a docker
symlink, so `container_engine: docker` already drives it.

**Running treeman inside a devcontainer.** treeman detects
`/.dockerenv`, `/run/.containerenv`, `REMOTE_CONTAINERS=true` and
`DEVCONTAINER=true`. When inside a container, the inspect path is
skipped if the engine socket isn't reachable — your configured
`host:` is used as-is, so on a shared compose network just write
the sibling service name (`host: db`) and it resolves via the
compose-provided DNS. As a last resort `host.docker.internal` is
probed before erroring.
