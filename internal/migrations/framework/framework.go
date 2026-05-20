// Package framework detects which migration framework owns a given
// repo. Ported from `crates/treeman-migrations/src/lib.rs`.
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

// Spec describes one migration framework.
type Spec struct {
	Name          string
	Markers       []string // each entry may be `a|b|c` (any-of)
	MigrationDirs []string // glob patterns relative to repo root
	FileGlobs     []string // glob patterns for migration files in those dirs
	Lockfiles     []string
	HashMode      HashMode
	OnModify      OnModify
	EngineHint    string // "mysql", "postgres", "" if unknown
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
// covers the 14 frameworks the Rust workspace ships. Users add
// custom detectors via the YAML `frameworks:` block.
type Registry struct {
	Specs []Spec
}

// DefaultRegistry returns the 14 built-in detectors.
func DefaultRegistry() *Registry {
	return &Registry{Specs: builtins()}
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
		},
		{
			Name:          "rails",
			Markers:       []string{"bin/rails", "Gemfile", "config/database.yml"},
			MigrationDirs: []string{"db/migrate", "engines/*/db/migrate"},
			FileGlobs:     []string{"*.rb"},
			Lockfiles:     []string{"Gemfile.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
		},
		{
			Name:          "django",
			Markers:       []string{"manage.py"},
			MigrationDirs: []string{"**/migrations"},
			FileGlobs:     []string{"[0-9]*_*.py"},
			Lockfiles:     []string{"Pipfile.lock", "poetry.lock", "requirements.txt"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
		},
		{
			Name:          "golang-migrate",
			Markers:       []string{"go.mod"},
			MigrationDirs: []string{"**/migrations", "services/*/migrations", "cmd/*/migrations"},
			FileGlobs:     []string{"*.up.sql"},
			Lockfiles:     []string{"go.sum"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
		},
		{
			Name:          "sqlx-cli",
			Markers:       []string{"Cargo.toml", "migrations"},
			MigrationDirs: []string{"migrations", "crates/*/migrations", "services/*/migrations"},
			FileGlobs:     []string{"*.sql"},
			Lockfiles:     []string{"Cargo.lock"},
			HashMode:      HashChecksum,
			OnModify:      OnDelta,
		},
		{
			Name:          "diesel",
			Markers:       []string{"diesel.toml"},
			MigrationDirs: []string{"migrations", "crates/*/migrations"},
			FileGlobs:     []string{"up.sql"},
			Lockfiles:     []string{"Cargo.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
		},
		{
			Name:    "prisma",
			Markers: []string{"prisma/schema.prisma"},
			MigrationDirs: []string{
				"prisma/migrations",
				"apps/*/prisma/migrations",
				"packages/*/prisma/migrations",
			},
			FileGlobs: []string{"migration.sql"},
			Lockfiles: []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:  HashChecksum,
			OnModify:  OnDelta,
		},
		{
			Name:          "knex",
			Markers:       []string{"knexfile.js|knexfile.ts|knexfile.cjs|knexfile.mjs"},
			MigrationDirs: []string{"migrations", "apps/*/migrations", "packages/*/migrations"},
			FileGlobs:     []string{"*.js", "*.ts"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
		},
		{
			Name:          "alembic",
			Markers:       []string{"alembic.ini"},
			MigrationDirs: []string{"**/versions"},
			FileGlobs:     []string{"*.py"},
			Lockfiles:     []string{"poetry.lock", "Pipfile.lock", "requirements.txt"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
		},
		{
			Name:          "flyway",
			Markers:       []string{"flyway.conf"},
			MigrationDirs: []string{"**/db/migration"},
			FileGlobs:     []string{"[VRU]*.sql"},
			HashMode:      HashChecksum,
			OnModify:      OnRebuild,
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
			FileGlobs: []string{"*.ts", "*.js"},
			Lockfiles: []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:  HashFilename,
			OnModify:  OnRebuild,
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
		},
		{
			Name:          "sequelize",
			Markers:       []string{".sequelizerc|.sequelizerc.js|.sequelizerc.cjs"},
			MigrationDirs: []string{"migrations", "apps/*/migrations", "packages/*/migrations"},
			FileGlobs:     []string{"*.js", "*.ts"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
		},
		{
			Name:          "mikro-orm",
			Markers:       []string{"mikro-orm.config.ts|mikro-orm.config.js|mikro-orm.config.cjs"},
			MigrationDirs: []string{"src/migrations", "apps/*/src/migrations", "packages/*/src/migrations"},
			FileGlobs:     []string{"Migration*.ts", "Migration*.js"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			HashMode:      HashFilename,
			OnModify:      OnRebuild,
		},
	}
}
