# Advanced — snapshots, frameworks

[← back to README](../README.md)

## Snapshot cache + GC

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

## Frameworks

`treeman fw detect` lists every framework treeman recognises in
the current repo. Built-in detectors:

| Framework | Marker(s) | Migration dir(s) |
|---|---|---|
| laravel | `artisan` | `database/migrations`, `app/Modules/*/Database/Migrations` |
| rails | `bin/rails`, `Gemfile`, `config/database.yml` | `db/migrate`, `engines/*/db/migrate` |
| django | `manage.py` | `**/migrations` |
| golang-migrate | `go.mod` | `**/migrations`, `services/*/migrations`, `cmd/*/migrations` |
| sqlx-cli | `Cargo.toml` + `migrations/` | `migrations`, `crates/*/migrations`, `services/*/migrations` |
| diesel | `diesel.toml` | `migrations`, `crates/*/migrations` |
| prisma | `prisma/schema.prisma` | `prisma/migrations`, `apps/*/prisma/migrations`, `packages/*/prisma/migrations` |
| knex | `knexfile.{js,ts,cjs,mjs}` | `migrations`, `apps/*/migrations`, `packages/*/migrations` |
| alembic | `alembic.ini` | `**/versions` |
| flyway | `flyway.conf` | `**/db/migration` |
| typeorm | `data-source.{ts,js}`, `ormconfig.*`, `typeorm.config.*` | `src/migrations`, `src/migration`, `migrations`, monorepo variants |
| drizzle | `drizzle.config.{ts,js,mjs,cjs,mts,json}` | `drizzle`, `apps/*/drizzle`, `packages/*/drizzle` |
| sequelize | `.sequelizerc{,.js,.cjs}` | `migrations`, `apps/*/migrations`, `packages/*/migrations` |
| mikro-orm | `mikro-orm.config.{ts,js,cjs}` | `src/migrations`, `apps/*/src/migrations`, `packages/*/src/migrations` |

All migration files, lockfiles, and dumps are content-hashed
(BLAKE3) — the legacy per-framework filename/checksum "hash mode" is
gone (it relied on an append-only convention that wasn't enforced, so
an in-place edit could keep a stale template alive). Per-directory
results are cached against the dir's `(mtime, file count)` so an
unchanged tree skips the re-read; any content/file change moves the
fingerprint and triggers a rebuild.

