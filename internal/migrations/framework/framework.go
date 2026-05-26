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

// Validator is an optional second-stage check a Spec can use when its
// Markers are ambiguous (e.g. `go.mod` matches every Go repo, not just
// repos that use golang-migrate). Returns true iff the repo really
// looks like the framework in question.
type Validator func(repoRoot string) bool

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
	// Validate, when non-nil, runs after the marker check and must
	// also pass for Detect to return true. Used to disambiguate
	// frameworks whose Markers are too coarse on their own (e.g.
	// `go.mod` for golang-migrate, which would otherwise match every
	// Go repo).
	Validate Validator `yaml:"-"`
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
	if s.Validate != nil && !s.Validate(repoRoot) {
		return false
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

// hasGolangMigrateEvidence reports whether repoRoot shows a signal
// that the project actually uses golang-migrate — beyond merely being
// a Go repo. Checks `go.sum` for the module import, then walks a few
// plausible migration locations looking for *.up.sql files.
func hasGolangMigrateEvidence(repoRoot string) bool {
	if b, err := os.ReadFile(filepath.Join(repoRoot, "go.sum")); err == nil {
		if strings.Contains(string(b), "github.com/golang-migrate/migrate") {
			return true
		}
	}
	for _, dir := range []string{"migrations", "db/migrations", "internal/migrations"} {
		matches, _ := filepath.Glob(filepath.Join(repoRoot, dir, "*.up.sql"))
		if len(matches) > 0 {
			return true
		}
	}
	return false
}

// dbNameEnv is the canonical MigrateEnv every non-Laravel spec uses.
// `DB_NAME` is the treeman convention (see every fw_* e2e test): the
// user references `${DB_NAME}` inside their MigrateRun connection
// string, or wires their config to read the var. The framework's own
// env-var conventions are too varied (and often non-existent) to put
// in a default scaffold — `DB_NAME` keeps the contract uniform and
// editor-friendly.
func dbNameEnv() map[string]string {
	return map[string]string{"DB_NAME": "{target_db}"}
}

func builtins() []Spec {
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
			MigrateEnv:    dbNameEnv(),
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
			MigrateEnv:    dbNameEnv(),
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
			MigrateEnv:    dbNameEnv(),
			// go.mod alone matches every Go repo, so require a second
			// signal: either the golang-migrate module is imported, or
			// the repo contains at least one *.up.sql file (the
			// framework's distinctive naming convention).
			Validate: hasGolangMigrateEvidence,
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
			MigrateEnv:    dbNameEnv(),
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
			MigrateEnv:    dbNameEnv(),
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
			MigrateEnv: dbNameEnv(),
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
			MigrateEnv:    dbNameEnv(),
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
			MigrateEnv:    dbNameEnv(),
		},
		{
			Name:          "flyway",
			Markers:       []string{"flyway.conf|flyway.toml"},
			MigrationDirs: []string{"**/db/migration"},
			FileGlobs:     []string{"[VRU]*.sql"},
			HashMode:      HashChecksum,
			OnModify:      OnRebuild,
			MigrateRun:    "flyway migrate",
			MigrateEnv:    dbNameEnv(),
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
			MigrateEnv: dbNameEnv(),
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
			MigrateEnv:    dbNameEnv(),
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
			MigrateEnv:    dbNameEnv(),
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
			MigrateEnv:    dbNameEnv(),
		},
	}
}
