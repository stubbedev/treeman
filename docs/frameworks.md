# Framework presets

[← back to docs](README.md)

Auto-generated from `framework.DefaultRegistry` (the built-in
migration-framework detectors). Run `just sync-docs` after adding a
preset to refresh. `treeman fw detect` lists which of these match the
current repo; declare your own under the `frameworks:` config block.

21 built-in detectors. Marker entries joined by `|` mean any-of.

| Framework | Marker(s) | Migration dir(s) | Engine hint |
|---|---|---|---|
| laravel | `artisan` | `database/migrations`, `app/Modules/*/Database/Migrations`, `app/Modules/*/Database/migrations`, `Modules/*/Database/Migrations`, `Modules/*/Database/migrations` | mysql |
| rails | `bin/rails`, `Gemfile`, `config/database.yml` | `db/migrate`, `engines/*/db/migrate` | — |
| django | `manage.py` | `**/migrations` | — |
| golang-migrate | `go.mod` | `**/migrations`, `services/*/migrations`, `cmd/*/migrations` | — |
| sqlx-cli | `Cargo.toml`, `migrations` | `migrations`, `crates/*/migrations`, `services/*/migrations` | — |
| diesel | `diesel.toml` | `migrations`, `crates/*/migrations` | — |
| prisma | `prisma/schema.prisma` | `prisma/migrations`, `apps/*/prisma/migrations`, `packages/*/prisma/migrations` | — |
| knex | `knexfile.js|knexfile.ts|knexfile.cjs|knexfile.mjs` | `migrations`, `apps/*/migrations`, `packages/*/migrations` | — |
| alembic | `alembic.ini` | `**/versions` | — |
| flyway | `flyway.conf|flyway.toml` | `**/db/migration`, `sql` | — |
| typeorm | `ormconfig.json|ormconfig.js|ormconfig.ts|ormconfig.yaml|ormconfig.yml|data-source.ts|data-source.js|typeorm.config.ts|typeorm.config.js` | `src/migrations`, `migrations`, `apps/*/src/migrations`, `packages/*/src/migrations` | — |
| drizzle | `drizzle.config.ts|drizzle.config.js|drizzle.config.mjs|drizzle.config.cjs|drizzle.config.mts|drizzle.config.json` | `drizzle`, `apps/*/drizzle`, `packages/*/drizzle` | — |
| sequelize | `.sequelizerc|.sequelizerc.js|.sequelizerc.cjs` | `migrations`, `apps/*/migrations`, `packages/*/migrations` | — |
| mikro-orm | `mikro-orm.config.ts|mikro-orm.config.js|mikro-orm.config.cjs` | `migrations`, `src/migrations`, `apps/*/migrations`, `apps/*/src/migrations`, `packages/*/migrations`, `packages/*/src/migrations` | — |
| doctrine-migrations | `composer.json` | `migrations`, `src/Migrations` | — |
| goose | `go.mod` | `migrations`, `db/migrations`, `internal/db/migrations`, `cmd/*/migrations` | — |
| liquibase | `liquibase.properties|liquibase.yaml|liquibase.yml|liquibase.json` | `db/changelog`, `src/main/resources/db/changelog`, `changelog`, `changelogs` | — |
| ef-core | `.` | `Migrations`, `*/Migrations`, `src/*/Migrations` | — |
| ecto | `mix.exs` | `priv/repo/migrations`, `apps/*/priv/*/migrations` | — |
| dbmate | `db/migrations` | `db/migrations` | — |
| atlas | `atlas.hcl|migrations/atlas.sum` | `migrations` | — |

Every matched migration file, lockfile, and dump is content-hashed
(BLAKE3) into the snapshot fingerprint — there is no per-framework
"hash mode"; any content/add/remove triggers a rebuild.
