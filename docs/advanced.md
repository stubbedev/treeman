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

| Framework | Marker(s) | Migration dir(s) | Hash mode |
|---|---|---|---|
| laravel | `artisan` | `database/migrations`, `app/Modules/*/Database/Migrations` | filename |
| rails | `bin/rails`, `Gemfile`, `config/database.yml` | `db/migrate`, `engines/*/db/migrate` | filename |
| django | `manage.py` | `**/migrations` | filename |
| golang-migrate | `go.mod` | `**/migrations`, `services/*/migrations`, `cmd/*/migrations` | filename |
| sqlx-cli | `Cargo.toml` + `migrations/` | `migrations`, `crates/*/migrations`, `services/*/migrations` | checksum |
| diesel | `diesel.toml` | `migrations`, `crates/*/migrations` | filename |
| prisma | `prisma/schema.prisma` | `prisma/migrations`, `apps/*/prisma/migrations`, `packages/*/prisma/migrations` | checksum |
| knex | `knexfile.{js,ts,cjs,mjs}` | `migrations`, `apps/*/migrations`, `packages/*/migrations` | filename |
| alembic | `alembic.ini` | `**/versions` | filename |
| flyway | `flyway.conf` | `**/db/migration` | checksum |
| typeorm | `data-source.{ts,js}`, `ormconfig.*`, `typeorm.config.*` | `src/migrations`, `src/migration`, `migrations`, monorepo variants | filename |
| drizzle | `drizzle.config.{ts,js,mjs,cjs,mts,json}` | `drizzle`, `apps/*/drizzle`, `packages/*/drizzle` | checksum |
| sequelize | `.sequelizerc{,.js,.cjs}` | `migrations`, `apps/*/migrations`, `packages/*/migrations` | filename |
| mikro-orm | `mikro-orm.config.{ts,js,cjs}` | `src/migrations`, `apps/*/src/migrations`, `packages/*/src/migrations` | filename |

`HashFilename` mode skips file IO (Laravel/Rails/Django don't
mutate migrations; new files alone change the hash). `HashChecksum`
hashes contents (sqlx-cli / Prisma / Drizzle / Flyway mutate in place).

