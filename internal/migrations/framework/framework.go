// Package framework detects which migration framework owns a given
// repo.
//
// Each detector declares marker files (filesystem paths that must
// exist — `|`-separated alternatives are accepted), migration-dir
// glob patterns, file globs, lockfile globs, and a hash mode + on-
// modify policy.
package framework

import (
	"os"
	"path/filepath"
	"strings"
)

// HashMode determines how migration files are fingerprinted.
type HashMode string

const (
	// HashFilename — fingerprint the filename. Cheaper; fine when
	// migrations are write-once-immutable (Laravel, Rails, Django,
	// Knex, Drizzle, etc.).
	HashFilename HashMode = "filename"
	// HashChecksum — fingerprint the file contents. Needed for
	// frameworks that mutate migrations in place (sqlx-cli, Flyway).
	HashChecksum HashMode = "checksum"
)

// OnModify gates the watcher dispatch.
type OnModify string

const (
	OnRebuild OnModify = "rebuild"
	OnDelta   OnModify = "delta"
)

// Spec describes one migration framework. Consumed only by
// `treeman init` (scaffolding) and `treeman fw detect` (diagnostic) —
// nothing at runtime dispatches off a Spec, so these fields are pure
// templating data.
type Spec struct {
	Name          string
	Markers       []string // each entry may be `a|b|c` (any-of)
	MigrationDirs []string // glob patterns relative to repo root
	FileGlobs     []string // glob patterns for migration files in those dirs
	Lockfiles     []string
	HashMode      HashMode
	OnModify      OnModify
	EngineHint    string // "mysql", "postgres", "" if unknown
	// MigrateRun is the shell command `treeman init` writes into the
	// scaffolded `migrations.migrate.run` field. Example:
	// `php artisan migrate --force`.
	MigrateRun string
	// MigrateEnv is the env-var override map `treeman init` writes
	// into `migrations.migrate.env`. Each value uses the
	// `{target_db}` placeholder so the runtime substitutes the
	// per-run template DB name. Empty for frameworks that read the
	// target DB from their own config rather than env.
	MigrateEnv map[string]string
}

// Detect returns true iff every marker group in spec.Markers has at
// least one alternative present in repoRoot.
func (s Spec) Detect(repoRoot string) bool {
	if len(s.Markers) == 0 {
		return false
	}
	for _, group := range s.Markers {
		matched := false
		for _, alt := range strings.Split(group, "|") {
			alt = strings.TrimSpace(alt)
			if alt == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(repoRoot, alt)); err == nil {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// Registry — the ordered list of detectors. Built-in registry below
// covers the 14 frameworks treeman ships with. Users add custom
// detectors via the YAML `frameworks:` block.
type Registry struct {
	Specs []Spec
}

// DefaultRegistry returns the 14 built-in detectors.
func DefaultRegistry() *Registry {
	return &Registry{Specs: builtins()}
}

// LookupBuiltin returns the built-in Spec for `name` (e.g. "laravel",
// "rails") and reports whether one exists. Used by scaffolding paths
// (`treeman init`, `treeman fw detect`) to fetch a preset by name.
// No runtime caller — runtime behavior is driven entirely by the YAML.
func LookupBuiltin(name string) (Spec, bool) {
	if name == "" {
		return Spec{}, false
	}
	for _, s := range builtins() {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// DetectAll returns every Spec whose markers match.
func (r *Registry) DetectAll(repoRoot string) []Spec {
	var out []Spec
	for _, s := range r.Specs {
		if s.Detect(repoRoot) {
			out = append(out, s)
		}
	}
	return out
}

func builtins() []Spec {
	genericDBNameEnv := func() map[string]string {
		return map[string]string{"DATABASE_NAME": "{target_db}"}
	}
	return []Spec{
		{
			Name:    "laravel",
			Markers: []string{"artisan"},
			MigrationDirs: []string{
				"database/migrations",
				"app/Modules/*/Database/Migrations",
				"app/Modules/*/Database/migrations",
				"Modules/*/Database/Migrations",
				"Modules/*/Database/migrations",
			},
			FileGlobs:  []string{"*.php"},
			Lockfiles:  []string{"composer.lock"},
			HashMode:   HashFilename,
			OnModify:   OnRebuild,
			EngineHint: "mysql",
			MigrateRun: "php artisan migrate --force",
			MigrateEnv: map[string]string{
				"DB_DATABASE":      "{target_db}",
				"DB_TEST_DATABASE": "{target_db}",
			},
		},
		{
			Name:          "rails",
			Markers:       []string{"bin/rails", "Gemfile", "config/database.yml"},
			MigrationDirs: []string{"db/migrate", "engines/*/db/migrate"},
			FileGlobs:     []string{"*.rb"},
			Lockfiles:     []string{"Gemfile.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
			MigrateRun:    "bin/rails db:migrate",
			MigrateEnv:    map[string]string{"DATABASE": "{target_db}"},
		},
		{
			Name:          "django",
			Markers:       []string{"manage.py"},
			MigrationDirs: []string{"**/migrations"},
			FileGlobs:     []string{"[0-9]*_*.py"},
			Lockfiles:     []string{"Pipfile.lock", "poetry.lock", "requirements.txt"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
			MigrateRun:    "python manage.py migrate --noinput",
			MigrateEnv:    map[string]string{"DJANGO_DB_NAME": "{target_db}"},
		},
		{
			Name:          "golang-migrate",
			Markers:       []string{"go.mod"},
			MigrationDirs: []string{"**/migrations", "services/*/migrations", "cmd/*/migrations"},
			FileGlobs:     []string{"*.up.sql"},
			Lockfiles:     []string{"go.sum"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
			MigrateRun:    "migrate up",
			MigrateEnv:    map[string]string{"MIGRATE_DATABASE_NAME": "{target_db}"},
		},
		{
			Name:          "sqlx-cli",
			Markers:       []string{"Cargo.toml", "migrations"},
			MigrationDirs: []string{"migrations", "crates/*/migrations", "services/*/migrations"},
			FileGlobs:     []string{"*.sql"},
			Lockfiles:     []string{"Cargo.lock"},
			HashMode:      HashChecksum,
			OnModify:      OnDelta,
			MigrateRun:    "sqlx migrate run",
			MigrateEnv:    map[string]string{"DATABASE_URL_NAME": "{target_db}"},
		},
		{
			Name:          "diesel",
			Markers:       []string{"diesel.toml"},
			MigrationDirs: []string{"migrations", "crates/*/migrations"},
			FileGlobs:     []string{"up.sql"},
			Lockfiles:     []string{"Cargo.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
			MigrateRun:    "diesel migration run",
			MigrateEnv:    genericDBNameEnv(),
		},
		{
			Name:    "prisma",
			Markers: []string{"prisma/schema.prisma"},
			MigrationDirs: []string{
				"prisma/migrations",
				"apps/*/prisma/migrations",
				"packages/*/prisma/migrations",
			},
			FileGlobs:  []string{"migration.sql"},
			Lockfiles:  []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:   HashChecksum,
			OnModify:   OnDelta,
			MigrateRun: "npx prisma migrate deploy",
			MigrateEnv: genericDBNameEnv(),
		},
		{
			Name:          "knex",
			Markers:       []string{"knexfile.js|knexfile.ts|knexfile.cjs|knexfile.mjs"},
			MigrationDirs: []string{"migrations", "apps/*/migrations", "packages/*/migrations"},
			FileGlobs:     []string{"*.js", "*.ts"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
			MigrateRun:    "npx knex migrate:latest",
			MigrateEnv:    genericDBNameEnv(),
		},
		{
			Name:          "alembic",
			Markers:       []string{"alembic.ini"},
			MigrationDirs: []string{"**/versions"},
			FileGlobs:     []string{"*.py"},
			Lockfiles:     []string{"poetry.lock", "Pipfile.lock", "requirements.txt"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
			MigrateRun:    "alembic upgrade head",
			MigrateEnv:    genericDBNameEnv(),
		},
		{
			Name:          "flyway",
			Markers:       []string{"flyway.conf"},
			MigrationDirs: []string{"**/db/migration"},
			FileGlobs:     []string{"[VRU]*.sql"},
			HashMode:      HashChecksum,
			OnModify:      OnRebuild,
			MigrateRun:    "flyway migrate",
			MigrateEnv:    genericDBNameEnv(),
		},
		{
			Name: "typeorm",
			Markers: []string{
				"ormconfig.json|ormconfig.js|ormconfig.ts|ormconfig.yaml|ormconfig.yml|data-source.ts|data-source.js|typeorm.config.ts|typeorm.config.js",
			},
			MigrationDirs: []string{
				"src/migrations",
				"src/migration",
				"migrations",
				"apps/*/src/migrations",
				"packages/*/src/migrations",
			},
			FileGlobs:  []string{"*.ts", "*.js"},
			Lockfiles:  []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:   HashFilename,
			OnModify:   OnRebuild,
			MigrateRun: "npx typeorm migration:run",
			MigrateEnv: genericDBNameEnv(),
		},
		{
			Name: "drizzle",
			Markers: []string{
				"drizzle.config.ts|drizzle.config.js|drizzle.config.mjs|drizzle.config.cjs|drizzle.config.mts|drizzle.config.json",
			},
			MigrationDirs: []string{"drizzle", "apps/*/drizzle", "packages/*/drizzle"},
			FileGlobs:     []string{"*.sql"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:      HashChecksum,
			OnModify:      OnDelta,
			MigrateRun:    "npx drizzle-kit migrate",
			MigrateEnv:    genericDBNameEnv(),
		},
		{
			Name:          "sequelize",
			Markers:       []string{".sequelizerc|.sequelizerc.js|.sequelizerc.cjs"},
			MigrationDirs: []string{"migrations", "apps/*/migrations", "packages/*/migrations"},
			FileGlobs:     []string{"*.js", "*.ts"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
			MigrateRun:    "npx sequelize db:migrate",
			MigrateEnv:    genericDBNameEnv(),
		},
		{
			Name:          "mikro-orm",
			Markers:       []string{"mikro-orm.config.ts|mikro-orm.config.js|mikro-orm.config.cjs"},
			MigrationDirs: []string{"src/migrations", "apps/*/src/migrations", "packages/*/src/migrations"},
			FileGlobs:     []string{"Migration*.ts", "Migration*.js"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
			MigrateRun:    "npx mikro-orm migration:up",
			MigrateEnv:    genericDBNameEnv(),
		},
	}
}
