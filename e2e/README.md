# e2e tests

End-to-end suite that drives treeman against real engines + real
framework toolchains booted via docker-compose. Each subdirectory
owns one compose file and one Go test file gated with
`//go:build e2e` so the default `go test ./...` skips them.

## Running

```sh
just test-e2e
# or
go test -tags=e2e ./e2e/... -timeout 60m
```

First run pulls ~6 GB of images and compiles ~2 GB of Rust deps
(sqlx-cli + diesel_cli). Subsequent runs are container-cached.

Tests run sequentially against the host's docker daemon — each suite
binds a unique host port (13306–15562, 16380, 19200–19201, 27117–27118)
so you don't need `-p 1`.

## Coverage matrix

### Engine suites — every engine through a full prep cycle

| Suite | Engine | What it asserts |
|---|---|---|
| `mysql/` | MySQL 8.4 | Cold build → cache hit → cold rebuild on input edit |
| `postgres/` | Postgres 16 | Same cycle |
| `mongo/` | MongoDB 7 | Seed → snapshot → cache hit cycle |
| `redis/` | Redis 7 | Prefix-keyed template + fingerprint change |
| `elasticsearch/` | ES 8.15 | NDJSON `_bulk` dump → snapshot → fanout |

### Connection method suites

| Suite | What it asserts |
|---|---|
| `containerref/` | MySQL via `container:` ref — both published-port AND bridge-IP fallback |
| `containerref-all/` | Same 2 modes × all 5 engines (10 subtests) |

### Feature suites

| Suite | What it asserts |
|---|---|
| `compression/` | All 5 dump formats (plain / gzip / zstd / bzip2 / xz) → MySQL identical-content load |
| `watcher/` | fsnotify dispatch labels + burst-debounce coalescing |
| `deltawatch/` | Input edit → `FinalizeWorktreeForWatch` → new fingerprint persisted (MySQL) |
| `lifecycle/` | `on-create-before-engines` → engine prepare → `on-create-after-engines` order; delete pair |
| `headwatcher/` | `git checkout` between branches fires HEAD watcher with new ref |
| `onfilechange/` | Full daemon-managed watcher → `on-file-change` action with all env vars |
| `fanout/` | `test_clones: 4` produces 4 populated clones; cache-hit on rerun |
| `switchback/` | Worktree A → B → A → B cycle; revisits are cache hits, both fingerprints survive |
| `teardown/` | `TeardownWorktree` drops source + clones, **preserves** the cached template |

### Framework suites — every detected framework through real migrate CLI

All 14 frameworks treeman's preset registry detects, each driven by
the actual upstream CLI against a real engine container:

| Suite | Framework | Runtime | Engine | Migrate CLI |
|---|---|---|---|---|
| `fw_laravel/` | Laravel | host PHP + composer | MySQL | `php artisan migrate` |
| `fw_rails/` | Rails / ActiveRecord | ruby:3.3 container | Postgres | `bin/rails db:migrate` |
| `fw_django/` | Django | python:3.12 container | Postgres | `python manage.py migrate` |
| `fw_alembic/` | Alembic | python:3.12 container | Postgres | `alembic upgrade head` |
| `fw_golang_migrate/` | golang-migrate | host `go install` | Postgres | `migrate up` |
| `fw_sqlx/` | sqlx-cli | rust container | Postgres | `sqlx migrate run` |
| `fw_diesel/` | Diesel | rust container | Postgres | `diesel migration run` |
| `fw_prisma/` | Prisma | node:20 container | Postgres | `prisma db push` |
| `fw_knex/` | Knex | node:20 container | Postgres | `knex migrate:latest` |
| `fw_typeorm/` | TypeORM | node:20 container | Postgres | `typeorm migration:run` |
| `fw_sequelize/` | Sequelize | node:20 container | Postgres | `sequelize-cli db:migrate` |
| `fw_drizzle/` | Drizzle | node:20 container | Postgres | `drizzle-kit push` |
| `fw_mikro/` | MikroORM | node:20 container | Postgres | `mikro-orm migration:up` |
| `fw_flyway/` | Flyway | flyway/flyway:10 image | Postgres | `flyway migrate` |

Each framework test:
1. Boots an engine + a runtime container (or installs locally where the toolchain exists on host).
2. Lays down a minimal framework-shaped project under `/tmp` (bind-mounted into the runtime container).
3. Wires `migrate.run = "docker exec <runtime> <cli> ..."` with `{target_db}` redirection through env vars.
4. Calls `prepare.Run` → asserts the framework's expected table exists in the per-worktree DB.

## Adding a new e2e

1. New subdir under `e2e/`
2. `docker-compose.yml` binding a fresh port number (see existing ones to pick the next free port)
3. `<name>_test.go` with `//go:build e2e` and a test that uses
   `harness.SkipIfNoDocker`, `harness.ComposeUp`, `harness.WaitForReady`,
   `harness.NewEnv` + `RunPrepare` or direct `daemon.*` calls
4. Run with `go test -tags=e2e -v ./e2e/<name>/...`

The harness deliberately keeps a thin surface — tests construct
`config.Config` in-memory and call `prepare.Run` directly when they
need fine-grained assertion control, OR write a `.treeman.yaml` and
invoke `daemon.FinalizeWorktree` when they want the full
hook-orchestration path.
