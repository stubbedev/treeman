# Advanced — snapshots, frameworks, binlog

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

## Binlog (MySQL delta replay)

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
