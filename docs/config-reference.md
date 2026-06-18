# Configuration reference

[← back to README](../README.md)

Auto-generated from the `config.Config` Go types (the same
reflection that produces `schemas/treeman.schema.json`).
Run `just sync-docs` after touching a config field to refresh.
For worked examples and guidance see [configuration.md](configuration.md).

## Config layers

treeman reads config from two files that merge into one effective config:

1. **User-global** — `~/.config/treeman/config.yaml` (`$XDG_CONFIG_HOME/treeman/config.yaml`). Machine-wide defaults shared by every repo. Scaffold it with `treeman init --global`.
2. **Per-repo** — `<repo>/.treeman.yaml` (plus an optional git-ignored `.treeman.local.yaml` overlay). Project-specific settings.

Later layers override earlier ones. Each top-level key has a **scope** that determines which file it may appear in — a key in the wrong file is a hard error at load time (no flag relaxes it):

- **global** — only valid in the user-global config.
- **repo** — only valid in a repo `.treeman.yaml`.
- **both** — valid in either; the global value is the default, a repo value overrides it.

| Key | Scope |
|-----|-------|
| `daemon` | global |
| `connections` | both |
| `snapshots` | global |
| `worktrees` | both |
| `env_sources` | repo |
| `patches` | repo |
| `databases` | repo |
| `hooks` | repo |
| `debounce_ms` | both |
| `frameworks` | both |
| `logs` | global |
| `auto_fetch` | both |
| `main_worktree` | repo |
| `ports` | both |
| `status` | global |
| `notifications` | global |

## Generated examples

Complete examples covering every key valid in each layer, generated from the schema (placeholder values — replace with real ones).

### User-global `~/.config/treeman/config.yaml`

```yaml
# Daemon process settings: stderr log level. Typically lives in
daemon:
    log_level: '...'
# Connection blocks per supported engine (MySQL, Postgres,
connections:
    mysql: '...'
    postgres: '...'
    mongodb: '...'
    redis: '...'
    elasticsearch: '...'
    s3:
        endpoint: '...'
        region: '...'
        access_key: '...'
        secret_key: '...'
        use_path_style: false
        container: '...'
        compose_service: '...'
        compose_project: '...'
        container_engine: '...'
        container_network: '...'
# Snapshot retention/eviction policy for cached post-migration
snapshots:
    cap_per_repo: 0
    keep_per_source: 0
    max_age_days: 0
    max_total_gb: 0
    gc_interval_minutes: 0
# Worktree creation behaviour: root path, symlinked mirrors
worktrees:
    root: '...'
    links:
        - '...'
    copies:
        - '...'
# DebounceMs is the file-watcher debounce window in
debounce_ms: 0
# User-defined migration frameworks keyed by name. Use this when
frameworks:
    <name>:
        markers:
            - '...'
        migration_dirs:
            - '...'
        file_pattern: '...'
        lockfiles:
            - '...'
        engine_hint: '...'
# Logs retention. Daemon-side prune drops rows older than
logs:
    keep_days: 0
# AutoFetch policy. Daemon-side periodic `git fetch --all --prune`
auto_fetch:
    enabled: false
    interval_minutes: 0
    mode: ff
# Ports declares per-worktree port slots. Each entry is a named
ports:
    <name>:
        - 0
# Status configures the `treeman status` widget output (icons,
status:
    icons:
        stable: '...'
        up: '...'
        down: '...'
        failed: '...'
    labels:
        stable: '...'
        up: '...'
        down: '...'
        failed: '...'
    separator: '...'
    header: '...'
    row: '...'
    main_marker: '...'
    formats:
        <name>: '...'
# Notifications opts into desktop notifications (notify-send on
notifications:
    enabled: false
    events:
        - stable
    backend: auto
```

### Per-repo `.treeman.yaml`

```yaml
# Connection blocks per supported engine (MySQL, Postgres,
connections:
    mysql: '...'
    postgres: '...'
    mongodb: '...'
    redis: '...'
    elasticsearch: '...'
    s3:
        endpoint: '...'
        region: '...'
        access_key: '...'
        secret_key: '...'
        use_path_style: false
        container: '...'
        compose_service: '...'
        compose_project: '...'
        container_engine: '...'
        container_network: '...'
# Worktree creation behaviour: root path, symlinked mirrors
worktrees:
    root: '...'
    links:
        - '...'
    copies:
        - '...'
# EnvSources is the ordered list of `.env*` files the credential
env_sources:
    - '...'
# Files to rewrite inside each worktree with per-worktree values
patches:
    - file: '...'
      format: dotenv
      set:
        <name>: '...'
# One entry per database the project owns. Each entry pairs an
databases:
    - engine: mysql
      name_template: '...'
      dump: '...'
      migrate:
        run: '...'
        env:
            <name>: '...'
      rollback:
        run: '...'
        env:
            <name>: '...'
      seed:
        run: '...'
        env:
            <name>: '...'
      inputs:
        - '...'
      test_clones:
        clones: auto
        name_template: '...'
      key_prefix: '...'
      fanout: 0
      prewarm: 0
      branch_scoped: false
# Lifecycle hooks fired around worktree create/delete/checkout and
hooks:
    create-before-engines:
        - run: '...'
          cwd: '...'
          container: '...'
          compose_service: '...'
          compose_project: '...'
          container_engine: '...'
    create-after-engines:
        - run: '...'
          cwd: '...'
          container: '...'
          compose_service: '...'
          compose_project: '...'
          container_engine: '...'
    delete-before-engines:
        - run: '...'
          cwd: '...'
          container: '...'
          compose_service: '...'
          compose_project: '...'
          container_engine: '...'
    delete-after-engines:
        - run: '...'
          cwd: '...'
          container: '...'
          compose_service: '...'
          compose_project: '...'
          container_engine: '...'
    checkout:
        - run: '...'
          cwd: '...'
          container: '...'
          compose_service: '...'
          compose_project: '...'
          container_engine: '...'
    file-change:
        - run: '...'
          cwd: '...'
          container: '...'
          compose_service: '...'
          compose_project: '...'
          container_engine: '...'
          match: '...'
# DebounceMs is the file-watcher debounce window in
debounce_ms: 0
# User-defined migration frameworks keyed by name. Use this when
frameworks:
    <name>:
        markers:
            - '...'
        migration_dirs:
            - '...'
        file_pattern: '...'
        lockfiles:
            - '...'
        engine_hint: '...'
# AutoFetch policy. Daemon-side periodic `git fetch --all --prune`
auto_fetch:
    enabled: false
    interval_minutes: 0
    mode: ff
# MainWorktree opts the repo's main checkout (repo root) into the
main_worktree:
    enabled: false
    databases:
        - name_template: '...'
          key_prefix: '...'
          test_clones:
            clones: auto
            name_template: '...'
          fanout: 0
# Ports declares per-worktree port slots. Each entry is a named
ports:
    <name>:
        - 0
```

## Top-level keys

### `daemon` *([DaemonConfig](#daemonconfig))* _[global]_

Daemon process settings: stderr log level. Typically lives in
the user-global config.

### `connections` *([ConnectionsConfig](#connectionsconfig))* _[both]_

Connection blocks per supported engine (MySQL, Postgres,
MongoDB, Redis, Elasticsearch). Treeman dials these to create
per-worktree clone databases, run migrations.

### `snapshots` *([SnapshotsConfig](#snapshotsconfig))* _[global]_

Snapshot retention/eviction policy for cached post-migration
template snapshots: per-repo cap, per-source keep, max age,
max total size, GC cadence.

### `worktrees` *([WorktreesConfig](#worktreesconfig))* _[both]_

Worktree creation behaviour: root path, symlinked mirrors
(links), and copied files (copies).

### `env_sources` *(array of string)* _[repo]_

EnvSources is the ordered list of `.env*` files the credential
resolver consults when looking up DB passwords and other
secrets. Later entries override earlier ones. Empty means no
env files are read — `treeman init` scaffolds a
framework-tailored `env_sources` list.
Per-worktree rewriting of these files lives in `patches:`.

### `patches` *(array of [Patch](#patch))* _[repo]_

Files to rewrite inside each worktree with per-worktree values
(slug-substituted DB names, cache prefixes, etc.). Supports
dotenv key=value files, phpunit.xml `<env>` blocks, generic
YAML, JSON, TOML, and INI. Each patched file is wired through
git's clean/smudge filter so the rewrite is hidden from
`git status` while still letting `git pull` / `git checkout`
overwrite the file on incoming changes (the smudge filter
re-applies the per-worktree value on the way back to the
working tree).

Re-applied on every `treeman wt finalize` so a branch switch
inside an existing worktree re-evaluates each patch against
the new HEAD's slug.

### `databases` *(array of [DatabaseConfig](#databaseconfig))* _[repo]_

One entry per database the project owns. Each entry pairs an
engine with a dump path, migration source, test-clone fanout,
and optional namespace template.

### `hooks` *([HooksConfig](#hooksconfig))* _[repo]_

Lifecycle hooks fired around worktree create/delete/checkout and
on watched-file changes. A flat block of trigger-keyed action
lists (create-before-engines, create-after-engines,
delete-before-engines, delete-after-engines, checkout,
file-change); see HooksConfig. Dispatched non-blocking via
the daemon; when the daemon is unreachable they run inline
(blocking) in the CLI.

### `debounce_ms` *(integer)* _[both]_

DebounceMs is the file-watcher debounce window in
milliseconds. Coalesces editor save bursts into one re-prep
dispatch. Default 500.

### `frameworks` *(map of name → [CustomFramework](#customframework))* _[both]_

User-defined migration frameworks keyed by name. Use this when
the built-in framework presets don't cover your tool — declare
the markers, migration dirs, file pattern, lockfiles, and
engine hint explicitly.

### `logs` *([LogsConfig](#logsconfig))* _[global]_

Logs retention. Daemon-side prune drops rows older than
`keep_days` from the events, hook_runs, and hook_log_chunks
tables on a fixed interval. Set 0 to keep forever (no prune).

### `auto_fetch` *([AutoFetchConfig](#autofetchconfig))* _[both]_

AutoFetch policy. Daemon-side periodic `git fetch --all --prune`
per registered repo, followed by a fast-forward (`merge
--ff-only @{u}`) or rebase per active worktree, per
`auto_fetch.mode`. Skips dirty trees, non-ff branches, and
upstreamless branches. Enabled by default at a 15-minute cadence.

### `main_worktree` *([MainWorktreeConfig](#mainworktreeconfig))* _[repo]_

MainWorktree opts the repo's main checkout (repo root) into the
same watcher-driven prepare/migrate/teardown lifecycle that
linked `.worktrees/<slug>` checkouts already get. Off by default
— flipping it on for an existing repo will start creating
per-branch databases when the user switches branches at the
repo root.

### `ports` *(map of name → [PortSpec](#portspec))* _[both]_

Ports declares per-worktree port slots. Each entry is a named
slot with a port range; treeman allocates a free port per slot
at `wt create` time and exposes it via the `{port_<name>}`
template token (usable in `patches[].set[*]` values).
Persisted in SQLite so the assignment survives across daemon
restarts; freed on `wt delete`.

Use slot names that match the role they fill in your app (e.g.
`octane`, `webpack`, `reverb`) — the name shows up in every
`{port_<name>}` reference and in `wt show` output.

### `status` *([StatusConfig](#statusconfig))* _[global]_

Status configures the `treeman status` widget output (icons,
labels, hover lines, custom bar formats). Lives in the global
config since the widget aggregates worktrees across every repo.

### `notifications` *([NotificationsConfig](#notificationsconfig))* _[global]_

Notifications opts into desktop notifications (notify-send on
Linux, the native banner via osascript on macOS) when a worktree
changes lifecycle state. Off by default. Lives in the global
config since the daemon that emits them is a single cross-repo
process.

## Types

### Action

One action: a `run` (string or list of strings) plus optional cwd + container wrapping.

#### `run` *(one of: string, array of string)* — **required**

Shell work for this action. String = single step; list = sequenced steps chained with `&&`. Required.

#### `cwd` *(string)*

Working directory for every step in this action. Relative paths resolve against the worktree root (host) or container WORKDIR (when wrapped).

#### `container` *(string)*

Container name or ID. When set, every step is wrapped in `<engine> exec`. Mutually exclusive with `compose_service`.

#### `compose_service` *(string)*

Docker Compose service name. Resolves the container via compose labels and wraps every step in `<engine> exec`. Mutually exclusive with `container`.

#### `compose_project` *(string)*

Docker Compose project name (`-p` flag). Defaults to $COMPOSE_PROJECT_NAME or parent dir name. Only meaningful with `compose_service`.

#### `container_engine` *(string)*

Container engine binary used for the exec wrap: `docker` (default), `podman`, `nerdctl`, `finch`.

### AutoFetchConfig

AutoFetchConfig — `auto_fetch:` block. Periodic daemon-side
`git fetch --all --prune` per registered repo, followed by a best-
effort `git merge --ff-only @{u}` per active worktree. Pull is
safe: it skips when the working tree is dirty, when the branch
has no upstream, or when the merge would not be a fast-forward.
Each failure logs a warning and the loop continues.

Use `enabled: false` (per-repo `.treeman.yaml` override) to opt a
repo out, e.g. when a separate scheduler already pulls or when
network policy forbids unsolicited fetches.

#### `enabled` *(boolean)*

Enabled toggles the loop. Default true. A pointer would let the
schema distinguish "unset" from "explicit false", but defaults
are applied centrally in applyDefaults and there is no
inheritance subtlety — a missing key means "use default".

#### `interval_minutes` *(integer)*

Cadence in minutes. Default 15. Minimum 1 (values below are
clamped at use-site to avoid a runaway tight loop on a typo).

#### `mode` *(string)*

Mode controls how a worktree's HEAD is advanced after fetch.
  - "ff" (default): `git merge --ff-only @{u}`. Refuses on
    divergence; user's unpushed commits are never touched.
  - "rebase": `git rebase --autostash @{u}`. Replays local
    commits on top of upstream. Auto-aborts on conflict so the
    working tree never lands in a half-rebased state.
Opt-in only — keep the safe ff path as the default.

_Allowed: `ff`, `rebase`_

### ClonesSetting

Number of test-clone databases to pre-warm. Either the literal string `auto` (treeman detects the test framework and uses the CPU count for per-worker runners) or a non-negative integer (0 disables pre-warming).

### ConnectionsConfig

ConnectionsConfig — `connections:` block.

#### `mysql` *([MysqlConn](#mysqlconn))*

MySQL / MariaDB / TiDB connection. Set when any `databases:`
entry uses one of those engines. Treeman dials this server to
create clones, dump templates.

#### `postgres` *([PostgresConn](#postgresconn))*

PostgreSQL connection. Required when any `databases:` entry
uses `engine: postgres` (or the `postgresql` alias).

#### `mongodb` *([MongoConn](#mongoconn))*

MongoDB connection. URI form (`mongodb://...`); host/port get
rewritten at dial time when a `container` ref is set.

#### `redis` *([RedisConn](#redisconn))*

Redis connection. URL form (`redis://...`); ContainerRef
semantics match MongoDB.

#### `elasticsearch` *([EsConn](#esconn))*

Elasticsearch / OpenSearch connection. HTTP URL form.

#### `s3` *([S3Conn](#s3conn))*

S3-compatible object storage connection (AWS S3, MinIO, Garage,
Ceph RGW, Backblaze B2, Cloudflare R2, ...). Required when any
`databases:` entry uses `engine: s3`. One connection serves many
per-worktree buckets named via the entry's `key_prefix`.

### CustomFramework

CustomFramework — `frameworks:` entry, lets users declare
migration frameworks treeman doesn't know about natively. Added to
the detector registry (via RegistryFor) consulted by `treeman fw
detect` and `treeman doctor`; `treeman init` and the MCP
`fw_detect` tool use only the built-in registry and ignore these
entries. At runtime treeman watches `databases[].inputs[]` directly.

#### `markers` *(array of string)* — **required**

Files (relative to repo root) whose presence indicates this
framework is in use. All markers must be present for `treeman
fw detect` / `treeman doctor` to recognise the framework.
Example: `["alembic.ini", "migrations/env.py"]`.

#### `migration_dirs` *(array of string)* — **required**

Glob patterns for the directories holding migration files.
Carried on the detection Spec reported by `treeman fw detect` /
`treeman doctor`; not emitted into `inputs[]` by `treeman init`
(init scaffolds only from built-in frameworks).

#### `file_pattern` *(string)* — **required**

Glob pattern for individual migration files within
`migration_dirs`. Example: `[0-9]*_*.py` (alembic) or
`V*__*.sql` (flyway).

#### `lockfiles` *(array of string)*

Lockfiles (e.g. `requirements.txt`, `pyproject.toml`,
`composer.lock`) carried on the detection Spec. To fold a
lockfile into the snapshot hash, declare it under
`databases[].inputs[]`.

#### `engine_hint` *(string)*

Optional hint about the database engine this framework
targets — `mysql`, `postgres`, etc. Carried on the detection
Spec; not used by `treeman init` (which scaffolds only from
built-in frameworks).

### DaemonConfig

DaemonConfig — `daemon:` block. Only `log_level` is user-tunable;
the socket path and event-log location are derived from
$XDG_RUNTIME_DIR / $XDG_STATE_HOME respectively and are not
configurable through YAML (one source of truth — the runtime
dirs the OS already manages).

#### `log_level` *(string)*

Log level for daemon stderr: `debug`, `info` (default),
`warn`, `error`. Read once at startup; reload by restarting
the daemon. Hook output is always captured regardless.

### DatabaseConfig

DatabaseConfig — one `databases:` entry. The `engine` discriminator
gates which sub-fields are valid.

`Fanout` is the optional override for the outer concurrency cap
during clone restore + DropMatching. Leave unset (omitempty / 0)
to use the safe per-engine default from internal/prepare. Raise
only if the server is provisioned for it (max_connections raised,
PG pg_database lock contention acceptable, etc.).

#### `engine` *(string)* — **required**

Engine discriminator. Gates which connection block is dialed
and which sub-fields (dump, migrations, namespaces) are valid.
`postgresql` is an alias for `postgres`; `opensearch` is an
alias for `elasticsearch`; `valkey` and `dragonfly` are aliases
for `redis` (same wire protocol, same key-prefix scoping).

_Allowed: `mysql`, `mariadb`, `tidb`, `postgres`, `postgresql`, `mongodb`, `redis`, `valkey`, `dragonfly`, `elasticsearch`, `opensearch`, `s3`_

#### `name_template` *(string)*

Template for the per-worktree database/index name. Supports
`{slug}`, `{slug_dash}`, `{slug_redis_queue}`, `{slug_redis_cache}`
(see the `template` package for definitions). Example:
`app_{slug}` → `app_feature-x`. Required for engines that
scope by database name (MySQL, Postgres, Mongo). Validated at
config-load time — typos fail loud.

#### `dump` *([DumpList](#dumplist))*

Source dump(s) used to seed the source DB before migrate/seed
run. Each path is relative to the repo root. Treeman hashes
every dump into the snapshot key, so changes invalidate the
cache. Three shapes:

  dump: seed.sql                       # bare string
  dump: { path: seed.sql, optional: true }  # mapping
  dump:                                # sequence
    - base.sql
    - { path: extras.sql, optional: true }

Sequence entries load in declared ORDER, so a base schema dump
followed by per-feature patches is supported without splicing
them into one file. Order matters for the fingerprint too —
reordering reproducible dumps is a content change.

#### `migrate` *([Step](#step))*

Migrate is the shell command that brings a freshly-loaded
source DB up to the current schema. Required when any input
glob matches migration files; optional otherwise (e.g. a DB
that's purely seed-driven).

#### `rollback` *([Step](#step))*

Rollback is the OPTIONAL shell command that unwinds the most
recently applied migrations, used to keep templates correct when
an *already-applied* migration's content is edited. Treeman runs
it against a clone of the prior template, then re-runs Migrate
forward — the only path that re-applies an edit to a migration
whose ledger row is baked into the source dump.

Treeman injects the number of migrations to unwind as the env var
TREEMAN_ROLLBACK_STEPS; reference it in Run, e.g.
  rollback: { run: "php artisan migrate:rollback --step=$TREEMAN_ROLLBACK_STEPS" }
(Run is passed verbatim to `sh -c`; only Env values are
`{placeholder}`-rendered, so use the shell env var, not a brace
placeholder.)

WARNING: rollback runs the migration's CURRENT down() — the edited
file on disk — not the original down() that matched the applied
schema. For edits that change down(), or for lossy/irreversible
down()s, this can produce a wrong or failing schema. Treeman
hard-falls-back to a full cold rebuild on ANY rollback or migrate
error, so a broken down() degrades to "slow but correct", never
"fast but wrong"-at-the-engine — but a down() that *succeeds* while
not faithfully inverting up() is the user's responsibility. Leave
this unset to disable the rollback path entirely (cold rebuild
only).

#### `seed` *([Step](#step))*

Seed is the shell command that populates non-migration data
(fixtures, ES mappings, Redis warm-cache keys, etc.). Runs
AFTER dump-load + migrate as the final cold-build step,
before treeman snapshots the populated state into the
template DB.

#### `inputs` *(array of [Input](#input))*

Inputs declare every file that determines this database's
template state. Each entry:
  1. Contributes a hash to the snapshot fingerprint (so any
     change auto-invalidates the cached template).
  2. Subscribes fsnotify so changes trigger a re-prep.
  3. Carries an optional `label:` that `hooks.file-change`
     actions can match against.

Glob patterns are repo-root-relative. Every matched file is
content-hashed (BLAKE3); a content, add, or remove moves the
fingerprint.

Cache-hit vs cold-build is derived purely from the input
hashes — there's no separate `on: rebuild` knob. If you want
to force a rebuild, change an input.

#### `test_clones` *([TestClonesSpec](#testclonesspec))*

Test-clone fanout: how many parallel database clones to
pre-warm for paratest/pytest-xdist/Jest workers/etc.

#### `key_prefix` *(string)*

KeyPrefix scopes every key/index a worktree creates under a
per-worktree prefix. Used by engines that don't scope by
database name — Redis (key prefix in DB 0) and Elasticsearch
/ OpenSearch (index-name prefix). Example: `{slug}:` →
`feature-x:`. The app must honour the prefix — Laravel's
`CACHE_PREFIX`, Rails `Rails.cache.options[:namespace]`,
ioredis `keyPrefix`, etc.

Supports the same placeholders as `name_template`: `{slug}`,
`{slug_dash}`, `{slug_redis_queue}`, `{slug_redis_cache}`.
Validated at config-load time.

#### `fanout` *(integer)*

Outer concurrency cap for clone restore + DropMatching.
Defaults to the per-engine safe value from internal/prepare.
Range 0–64. Raise only if the server is provisioned
(max_connections, PG pg_database lock contention, etc.).

_min: 0 · max: 64_

#### `prewarm` *(integer)*

Prewarm keeps N spare clones pre-restored from this database's
cached template (Postgres only — other engines have no
constant-time whole-database rename, so the knob is rejected at
config load). A cache-hit prepare claims a spare via
`ALTER DATABASE … RENAME` (milliseconds, size-independent)
instead of paying `CREATE DATABASE … TEMPLATE` per restore; a
detached replenisher then tops the pool back up. Spares are
named `<template>_spare<n>`, survive worktree teardown (they
belong to the template cache, not a worktree), and are dropped
with their template on snapshot eviction. Range 0–16; default
0 (off). Mutually exclusive with `branch_scoped`.

_min: 0 · max: 16_

#### `branch_scoped` *(boolean)*

BranchScoped turns this database into a git-for-databases
working copy: the app always talks to one stable ACTIVE
namespace, while treeman keeps a DURABLE per-branch copy of its
contents and swaps them in/out as the branch changes. One flag,
the whole lifecycle — no per-feature sub-fields, no
user-configured durable names (treeman derives them internally).

The active namespace is fixed per checkout, so the app's
connection string (`.env`) is patched once and never churns:
  - main worktree → the `main_worktree.databases[].name_template`
    overlay (typically a bare, unprefixed name the repo-root app
    already points at, e.g. `kontainer`).
  - linked worktree → `name_template` (or `key_prefix` for
    prefix-scoped engines) rendered against the worktree's
    branch-independent slug, so switching branches inside the
    worktree doesn't rename its DB.

Lifecycle, driven by HEAD changes + create/delete:
  - create / first switch onto a branch → seed the active
    namespace from the branch's own durable copy (resume) or,
    failing that, from its parent branch's data (tracked
    upstream, via the main overlay or a sibling worktree), or
    `dump.path`, or empty.
  - switch off a branch → capture the active namespace into that
    branch's durable copy first (manual data changes live on).
  - switch back → restore that durable copy. `treeman db reset`
    drops the durable copy + re-seeds from the live parent.

Engine-agnostic: name-scoped engines (MySQL, Postgres, MongoDB)
swap whole databases; prefix-scoped engines (Redis,
Elasticsearch/OpenSearch) swap the key/index namespace under the
rendered `key_prefix`. All five participate.

Mutually exclusive with `test_clones` / `fanout`: a branch_scoped
database is a stateful per-branch snapshot the app mutates in
place, not a reproducible source for throwaway parallel-test
clones. Config-load rejects the combination.

Postgres caveats (both stem from `CREATE DATABASE … TEMPLATE`,
which needs exclusive access to its source):
  - Capturing a branch on a switch briefly force-disconnects every
    session on the active database (the swap fences it and
    terminates backends), so the app momentarily loses its DB
    connections and must reconnect. Expected; not data loss.
  - Seeding a branch from its parent branch's LIVE database fails
    while other sessions are connected to the parent (treeman does
    NOT force-terminate another worktree's connections). Close
    those connections, or set `dump.path` to seed from the dump.
MySQL/MongoDB copy logically and have neither constraint.

### DatabaseOverlay

DatabaseOverlay holds the subset of `DatabaseConfig` fields that
can be tweaked per-context (main wt vs linked wt). Engine + Dump
are intentionally absent — changing engines per-context produces
schema chaos, and main / linked worktrees should always share the
same seed dump for snapshot-cache coherence.

Field semantics:

  - Strings: empty value means "inherit". Setting to a non-empty
    value replaces the base template.
  - TestClones: nil pointer means "inherit". A non-nil value
    replaces the entire spec (Clones + NameTemplate).
  - Fanout: nil pointer means "inherit". A non-nil value (including
    0) replaces the base — necessary because uint32(0) is a valid
    "use per-engine default" sentinel that's distinct from "no
    override".

#### `name_template` *(string)*

#### `key_prefix` *(string)*

#### `test_clones` *([TestClonesSpec](#testclonesspec))*

#### `fanout` *(integer)*

### DumpList

Source dump(s). Accepts a bare path string, a single mapping, or an ordered array of either.

### EsConn

elasticsearch connection — bare URL string OR structured object.

### FilteredAction

On-file-change action with optional label filter. Same shape as a hook Action plus a `match:` field (string or list).

#### `run` *(one of: string, array of string)* — **required**

Shell work for this action. String = single step; list = sequenced steps chained with `&&`. Required.

#### `cwd` *(string)*

Working directory for every step in this action. Relative paths resolve against the worktree root (host) or container WORKDIR (when wrapped).

#### `container` *(string)*

Container name or ID. When set, every step is wrapped in `<engine> exec`. Mutually exclusive with `compose_service`.

#### `compose_service` *(string)*

Docker Compose service name. Resolves the container via compose labels and wraps every step in `<engine> exec`. Mutually exclusive with `container`.

#### `compose_project` *(string)*

Docker Compose project name (`-p` flag). Defaults to $COMPOSE_PROJECT_NAME or parent dir name. Only meaningful with `compose_service`.

#### `container_engine` *(string)*

Container engine binary used for the exec wrap: `docker` (default), `podman`, `nerdctl`, `finch`.

#### `match` *(one of: string, array of string)*

Restrict this action to watch events carrying one of the named labels. Omit to fire for every event.

### HooksConfig

HooksConfig — `hooks:` block. A flat set of trigger-keyed action
lists. Each key's value is a list of Actions that fire when that
trigger happens. Actions in the same list run in parallel; the
trigger key itself encodes BOTH the lifecycle phase AND the timing
point, so there's no separate `when:` field anywhere.

Triggers (all optional — omit any you don't need):

  - create-before-engines — during `wt create`, after patches +
    bring-in (copies/links), BEFORE engine prepare. Standard
    home of dependency installs (composer/yarn/pip) so migrate
    can find vendor/.
  - create-after-engines — during `wt create`, after engine
    prepare. Use when actions need a populated database
    (cache warming, seed verification).
  - delete-before-engines — during `wt delete`, BEFORE DB
    drop. Graceful shutdown: drain queues, docker compose stop.
  - delete-after-engines — during `wt delete`, AFTER DB drop +
    git worktree remove. External notifications (Slack, CDN
    purge) that should announce only once the data is gone.
  - checkout — fires when the HEAD watcher sees a branch
    switch inside an existing worktree. Re-runs in addition to
    the regular finalize-on-HEAD-change behaviour.
  - file-change — fires when any `databases[].inputs[]` glob
    matches a filesystem event. Each action can optionally
    `match: <label>` to filter by the input entry's label.

Daemon execution is always non-blocking from the CLI's
perspective — each list of actions dispatches in parallel.

#### `create-before-engines` *(array of [Action](#action))*

OnCreateBeforeEngines — actions fire after worktree create +
patches + bring-in, before engine prepare.

#### `create-after-engines` *(array of [Action](#action))*

OnCreateAfterEngines — actions fire after engine prepare completes.

#### `delete-before-engines` *(array of [Action](#action))*

OnDeleteBeforeEngines — actions fire before DB drop on delete.

#### `delete-after-engines` *(array of [Action](#action))*

OnDeleteAfterEngines — actions fire after DB drop + worktree
remove on delete.

#### `checkout` *(array of [Action](#action))*

OnCheckout — actions fire when the HEAD watcher detects a
branch switch inside an existing worktree.

#### `file-change` *(array of [FilteredAction](#filteredaction))*

OnFileChange — actions fire when any `databases[].inputs[]`
glob matches a filesystem event. Each action can optionally
`match: <label>` to fire only when the matched input entry
carries that label; actions without `match:` fire for every
input event (any engine, any label).

The subprocess receives extra env vars naming the trigger:
  TREEMAN_WATCH_PATH   — absolute path that fired
  TREEMAN_WATCH_LABEL  — the label on the matched watch entry (or "")
  TREEMAN_WATCH_ENGINE — engine of the owning database (mysql, postgres, …)
  TREEMAN_WATCH_DB_NAME — rendered name_template of the owning database

### Input

One source of file state for the template fingerprint. Bare string OR full mapping.

### LogsConfig

LogsConfig — `logs:` block. One knob: how long to keep events +
hook_runs + their attached log chunks. The daemon's prune loop
applies the cutoff per table; chunks cascade via FK so dropping
a hook_runs row also drops its captured stdout/stderr.

#### `keep_days` *(integer)*

KeepDays sets the retention window in days. Rows older than
`now - keep_days * 24h` are removed on each daemon prune tick.
0 disables pruning (keep forever). Default 14.

### MainWorktreeConfig

MainWorktreeConfig — `main_worktree:` block. Opt-in handle that
promotes the repo root into a first-class worktree so the daemon's
HEAD watcher, file watcher, and prepare orchestration treat it the
same as a `.worktrees/<slug>` linked checkout. Off by default; flip
`enabled: true` to start producing per-branch databases at the
repo root.

One knob today (Enabled). The branch-switch policy is implicit:
FinalizeWorktree re-runs against the new branch's slug, which uses
the snapshot cache so a re-visit of any previously-seen branch is
a near-instant clone. Drop/keep policies will land as additional
fields once their teardown plumbing exists — premature wiring of
`on_branch_switch` would have shipped a config surface no code
consumed.

#### `enabled` *(boolean)*

Enabled toggles main-wt enrollment for this repo. Default false
— every existing install sees zero behaviour change until the
flag is set. Once true, the daemon ensures a worktrees row with
is_main=1 exists for the repo, spawns the per-wt HEAD + file
watchers against the repo root, and re-runs prepare on every
branch switch.

#### `databases` *(array of [DatabaseOverlay](#databaseoverlay))*

Databases is a sparse, index-aligned overlay over `databases:`.
Each entry's set fields replace the same field on the main-
indexed database when finalize runs against the main worktree;
unset fields inherit the top-level value. Linked worktrees are
untouched.

Common uses: a different `name_template` so the main wt's app
DB lives at `app_dev_{slug}` while linked wts use `app_{slug}`;
disabling test-clone fanout in main (`test_clones.clones: 0`)
because the main checkout doesn't run parallel test workers.

Overlay length must be <= len(databases). Entries beyond the
declared count are a config error caught at load time.

### MongoConn

mongodb connection — bare URL string OR structured object.

### MysqlConn

MySQL connection — bare DSN string OR structured object.

### NotificationsConfig

NotificationsConfig — `notifications:` block. Opt-in desktop
notifications fired by the daemon when a worktree crosses a lifecycle
status boundary. Backend is auto-detected per OS (notify-send on
Linux, `osascript -e 'display notification'` on macOS); platforms
without a known sender silently no-op.

Each notification is keyed to one of the four `treeman status`
buckets:
  - stable: a worktree finished preparing and is ready (finalize done)
  - up:     a worktree began preparing (finalize started)
  - down:   a worktree began tearing down
  - failed: a worktree's finalize errored

`up` and `down` are transient and chatty, so the default
(`events:` unset) only notifies on `stable` + `failed` — ready and
errored, the two resting states worth surfacing. Set `events:`
explicitly to opt into the transient ones, or to a subset.

#### `enabled` *(boolean)*

Enabled toggles the whole feature. Off by default — every
existing install sees zero behaviour change until it's set.

#### `events` *(array of string)*

Events is the set of status buckets that fire a notification.
Allowed values: stable, up, down, failed. When unset (nil) the
default of [stable, failed] applies (see applyDefaults). An
explicit empty list (`events: []`) disables every bucket while
leaving the feature otherwise "enabled" — useful as a base for a
per-repo override that re-adds buckets.

#### `backend` *(string)*

Backend forces a specific sender instead of OS auto-detection.
Allowed values: auto (default), notify-send, osascript, none.
`none` disables sending without unsetting `enabled` — handy to
mute notifications on one host while keeping the shared config.

_Allowed: `auto`, `notify-send`, `osascript`, `none`_

### Patch

Patch — one entry in the top-level `patches:` block. Each entry
targets one file under the worktree root and rewrites it with
per-worktree values via the `set:` map. Values are template
strings that accept `{slug}`, `{slug_dash}`,
`{slug_redis_queue}`, `{slug_redis_cache}`. Validated at
config-load time — unknown keys fail loud.

The driver is picked from `format:` when set, otherwise auto-
detected from the file extension:

	dotenv  — `.env`, `.env.*`
	phpunit — `.xml`, `.xml.dist` (phpunit.xml `<env>` blocks)
	yaml    — `.yaml`, `.yml`
	json    — `.json`
	toml    — `.toml`
	ini     — `.ini`, `.cfg`

Path syntax inside `set:` is uniform across drivers:

	dotenv / phpunit  — flat key (e.g. `DB_DATABASE`)
	ini               — `section.key` (top-level keys allowed too)
	yaml / json / toml — dotted path, optionally with `[N]` indices
	                    (e.g. `services[0].host`)

Each patched file is wired through git's clean/smudge filter so
the rewrite is hidden from `git status` while still letting
`git pull` / `git checkout` overwrite the file on incoming
changes — the daemon's HEAD watcher (or the smudge filter itself)
re-applies the patch after. The file must be tracked by git for
the filter to engage; gitignored files are patched in-place
without any git interaction.

#### `file` *(string)* — **required**

File path relative to the worktree root. Required.

#### `format` *(string)*

Driver name. Optional — leave unset to auto-detect from the
file extension. Explicit when the extension is ambiguous
(e.g. `phpunit` for a `.xml` that isn't standard XML).

_Allowed: `dotenv`, `phpunit`, `yaml`, `json`, `toml`, `ini`_

#### `set` *(object)*

Key → value-template map. Path syntax depends on the driver
(see the type doc-comment).

### PortSpec

Per-worktree port slot: an inclusive [min, max] range. Shorthand `[min, max]` is accepted; the long form is `{range: [min, max]}`.

### PostgresConn

Postgres connection — bare DSN string OR structured object.

### RedisConn

redis connection — bare URL string OR structured object.

### S3Conn

#### `endpoint` *(string)*

#### `region` *(string)*

#### `access_key` *(string)*

#### `secret_key` *(string)*

#### `use_path_style` *(boolean)*

#### `container` *(string)*

#### `compose_service` *(string)*

#### `compose_project` *(string)*

#### `container_engine` *(string)*

#### `container_network` *(string)*

### SnapshotsConfig

SnapshotsConfig — `snapshots:` block. Carries the retention /
eviction policies for cached engine templates. Snapshot state
lives entirely in SQLite + the engines themselves (template DBs);
no on-disk cache directory needs configuring.

`CapPerRepo` is the hard cap that triggers eviction on every
`RecordSnapshot`. LRU rows above the cap are dropped immediately
(in a background goroutine) so a busy worktree workflow never
accumulates unbounded cached templates per repo.

`KeepPerSource`, `MaxAgeDays`, `MaxTotalGb`, `GcIntervalMinutes`
drive the periodic daemon-side sweep; they're not consulted by
the inline-on-write eviction path.

#### `cap_per_repo` *(integer)*

Hard cap on cached snapshots per repository. Eviction runs
inline on every `RecordSnapshot`: rows above the cap (LRU
order) are dropped immediately in a background goroutine.
Default 8. Set to 0 to disable the inline cap (rely on the
periodic sweep only).

#### `keep_per_source` *(integer)*

Periodic-sweep policy: keep at most N snapshots per `source`
(a stable key derived from migration content). Default 500.

#### `max_age_days` *(integer)*

Periodic-sweep policy: drop snapshots older than N days.
Default 30.

#### `max_total_gb` *(integer)*

Periodic-sweep policy: drop oldest snapshots once the cache
dir exceeds N gigabytes on disk. Default 50.

#### `gc_interval_minutes` *(integer)*

Cadence (minutes) of the daemon's periodic snapshot-sweep
goroutine. Default 60.

### StatusBuckets

StatusBuckets carries one string per worktree bucket. Reused for
both the `icons` and `labels` maps so the four bucket names stay in
lockstep across the schema.

#### `stable` *(string)*

#### `up` *(string)*

#### `down` *(string)*

#### `failed` *(string)*

### StatusConfig

StatusConfig configures `treeman status` — the bar/waybar widget
that aggregates worktree health across every registered repo. Each
active worktree falls into one of four buckets:

	stable  — ready (last finalize succeeded, or never ran)
	up      — being prepared (finalize in progress)
	down    — being torn down (teardown in progress)
	failed  — last finalize errored

All knobs below feed the `{key}` template syntax used elsewhere in
`.treeman.yaml` (no separate templating engine). The built-in
`--format` values are `icon`, `hover`, `waybar`, and `json`; entries
in `formats` add or override named single-line formats.

#### `icons` *([StatusBuckets](#statusbuckets))*

Icons holds the glyph for each bucket. Exposed to format
templates as `{icon_stable}` / `{icon_up}` / `{icon_down}` /
`{icon_failed}`, plus `{icon}` for the worst non-empty bucket.
Defaults to Nerd Font glyphs; set an icon to a single space to
suppress it (an empty string falls back to the default).

#### `labels` *([StatusBuckets](#statusbuckets))*

Labels holds the text label for each bucket. Exposed as
`{label_stable}` etc. Defaults to the bucket name.

#### `separator` *(string)*

Separator joins the segments of the built-in `icon` line and is
exposed to templates as `{sep}`. Default " | ".

#### `header` *(string)*

Header is the `{key}` template for each repo heading in the
built-in `hover` format. Tokens: `{repo}`, `{total}`,
`{stable}`, `{up}`, `{down}`, `{failed}` (repo-scoped counts).
Default "{repo}  ({total})".

#### `row` *(string)*

Row is the `{key}` template for each worktree line in the
built-in `hover` format. Tokens: `{branch}`, `{slug}`,
`{state}`, `{bucket}`, `{main}`, `{state_suffix}`, `{path}`,
`{icon}`. Default "  {main}{branch}{state_suffix}".

#### `main_marker` *(string)*

MainMarker is substituted for `{main}` on a repo's main-worktree
row (empty string on linked worktrees). Default "★ ".

#### `formats` *(object)*

Formats declares named single-line `{key}` templates selectable
with `treeman status --format <name>`. A name matching the
built-in `icon` line overrides it; the structured built-ins
`hover`/`waybar`/`json` are reserved and cannot be overridden.
Available tokens match
the `icon` line: `{total}`, `{stable}`, `{up}`, `{down}`,
`{failed}`, `{icon_*}`, `{icon}`, `{label_*}`, `{class}`,
`{sep}`. A flat template cannot express the multi-line hover
body — customize that with `header` / `row` instead.

### Step

Step is one user-declared shell command executed against a target
database. Used by both `databases[].migrate:` and
`databases[].seed:` — the same shape, different lifecycle slots.

The framework's CLI reads its target DB from its own config — for
most stacks that's a connection string the user already keeps in
`.env` (or wires up in `config/database.yml`, `data-source.ts`,
etc.). Treeman builds per-worktree template databases with names
like `myapp_template_feature-x` and needs to redirect the command
at *that* DB, not the one the committed `.env` references. The
`Env` map says which env-var names to override; values are
rendered through the same `template` pass that produces the
per-run DB name. The scaffold convention is to set `DB_NAME`
(Laravel's preset uses the framework-native `DB_DATABASE`); the
user weaves `${DB_NAME}` into the relevant slot of their Run
command (typically inside a DSN) or wires their config to read
it. Supported substitutions:

	{target_db}         — resolved per-run database name / key prefix
	{slug}              — the slug value
	{slug_dash}         — slug with underscores → hyphens
	{slug_redis_queue}  — slug-derived Redis queue DB index (6..15)
	{slug_redis_cache}  — slug-derived Redis cache DB index (6..15)

Unknown keys fail loud at config-load time. Treeman also exports
`TREEMAN_TARGET_DB` to the subprocess unconditionally as a safety
net for tooling that wants the resolved name without a custom env
mapping.

#### `run` *(string)* — **required**

Run is the shell command treeman invokes via `sh -c`.
Required; an empty Run aborts `prepare` with a clear error.

#### `env` *(object)*

Env is a map of env-var names to value templates. Each entry
is set on the subprocess (overriding the framework's config
file). See the type doc-comment for the full list of supported
`{placeholder}` keys; literal values pass through unchanged.

### TestClonesSpec

TestClonesSpec — `test_clones:` sub-block. Used by every parallel
test runner (paratest, pest, pytest-xdist, Jest workers, Go
`-parallel`, cargo nextest, …). `clones` is either `auto` (treeman
detects the test framework and uses the CPU count for per-worker
runners) or an explicit integer.

#### `clones` *([ClonesSetting](#clonessetting))*

Number of test-clone databases to pre-warm. `auto` detects the
project's test framework and pre-warms one clone per CPU
(`runtime.NumCPU`) when that framework clones per-worker,
otherwise 1 (falling back to the CPU count when no framework is
detected). Explicit integer overrides; 0 disables pre-warming
entirely.

#### `name_template` *(string)* — **required**

Template for clone database names. Supports the same
placeholders as `databases[].name_template` plus `{n}` —
the 1-based clone index (only valid here). Required.
Example: `app_{slug}_test_{n}`.

### WorktreesConfig

WorktreesConfig — `worktrees:` block.

#### `root` *(string)*

Path (relative to the main worktree) where new worktrees are
created. Defaults to `.worktrees`. Override with e.g.
`../foo-worktrees` for the sibling-dir convention.

#### `links` *(array of string)*

Paths (relative to the main worktree) to *symlink* into each
new worktree on create. Use for committed-in-main-only caches
that the worktree should read but never mutate per-branch —
e.g. `node_modules`, `vendor`. The symlink points at the main
worktree's copy so all worktrees share one on-disk cache.
Glob meta-characters expand against the main worktree root.

#### `copies` *(array of string)*

Paths (relative to the main worktree) to *copy* into each new
worktree on create. Use for gitignored files that the worktree
needs in its own copy so it can be patched per-branch without
affecting the main worktree's copy — e.g. `.env`, `.env.local`.
Glob meta-characters expand against the main worktree root.
Directories are recursed; existing destinations are left alone
(idempotent re-runs).

