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
	"sort"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
)

// Validator is an optional second-stage check a Spec can use when its
// Markers are ambiguous (e.g. `go.mod` matches every Go repo, not just
// repos that use golang-migrate). Returns true iff the repo really
// looks like the framework in question.
type Validator func(repoRoot string) bool

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
	//
	// `json:"-"` is load-bearing, not cosmetic: a func type has no JSON
	// schema, and the MCP server reflects Spec into the `fw_detect`
	// output schema at startup (go-sdk AddTool). Without the tag that
	// reflection panics and the entire `treeman mcp` server fails to
	// boot. Mirrors testfw.Spec.Detect.
	Validate Validator `yaml:"-" json:"-"`
}

// Detect returns true iff every marker group in spec.Markers has at
// least one alternative present in repoRoot.
func (s Spec) Detect(repoRoot string) bool {
	if len(s.Markers) == 0 {
		return false
	}
	for _, group := range s.Markers {
		matched := false
		for alt := range strings.SplitSeq(group, "|") {
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

// RegistryFor returns the built-in detectors PLUS any user-defined
// frameworks from `cfg.Frameworks` (the `frameworks:` YAML block). A
// custom entry detects when all its markers are present (Spec.Detect).
// This is what makes the `frameworks:` block live — `treeman fw detect`
// and the doctor consult it so a project on a framework treeman doesn't
// ship a preset for is still recognised.
func RegistryFor(cfg *config.Config) *Registry {
	r := DefaultRegistry()
	if cfg == nil || len(cfg.Frameworks) == 0 {
		return r
	}
	names := make([]string, 0, len(cfg.Frameworks))
	for n := range cfg.Frameworks {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic detection order
	for _, name := range names {
		cf := cfg.Frameworks[name]
		spec := Spec{
			Name:          name,
			Markers:       cf.Markers,
			MigrationDirs: cf.MigrationDirs,
			Lockfiles:     cf.Lockfiles,
			EngineHint:    cf.EngineHint,
		}
		if cf.FilePattern != "" {
			spec.FileGlobs = []string{cf.FilePattern}
		}
		r.Specs = append(r.Specs, spec)
	}
	return r
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

// dbNameEnv is the MigrateEnv fallback for frameworks without a
// strong native DSN env-var convention. The user references
// `${DB_NAME}` inside their MigrateRun connection string, or wires
// their config to read the var. Matches the convention every fw_*
// e2e test uses.
func dbNameEnv() map[string]string {
	return map[string]string{"DB_NAME": "{target_db}"}
}

// dbURLEnv scaffolds DATABASE_URL for frameworks that natively read
// one (Prisma, Drizzle's CLI, sqlx, diesel, golang-migrate). The URL
// embeds `{target_db}` so treeman substitutes the per-run template
// DB name; the user replaces the user/password/host/port portion.
// Postgres is the default scheme — most URL-driven ecosystems lean
// postgres — but the user is expected to edit the prefix if needed.
func dbURLEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://user:password@127.0.0.1:5432/{target_db}?sslmode=disable",
	}
}

// hasGooseEvidence reports whether the repo imports pressly/goose.
// Lets us share `go.mod` as a marker with golang-migrate without
// collision — only one of the two validators returns true for a
// given project.
func hasGooseEvidence(repoRoot string) bool {
	b, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "github.com/pressly/goose")
}

// hasDoctrineMigrationsDep confirms a PHP project actually pulls in
// the doctrine-migrations-bundle (or the standalone library).
// composer.json alone is too broad — every PHP project has one.
func hasDoctrineMigrationsDep(repoRoot string) bool {
	b, err := os.ReadFile(filepath.Join(repoRoot, "composer.json"))
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "doctrine/doctrine-migrations-bundle") ||
		strings.Contains(s, "doctrine/migrations")
}

// hasEctoEvidence confirms a Mix project actually uses Ecto. mix.exs
// matches every Elixir project; the real signal is either ecto_sql
// in mix.lock or a default priv/repo/migrations directory.
func hasEctoEvidence(repoRoot string) bool {
	if b, err := os.ReadFile(filepath.Join(repoRoot, "mix.lock")); err == nil {
		if strings.Contains(string(b), `"ecto_sql"`) {
			return true
		}
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "priv", "repo", "migrations")); err == nil {
		return true
	}
	return false
}

// hasDBmateEvidence looks for DBmate's distinctive `-- migrate:up`
// directive inside files under `db/migrations/`. DBmate ships no
// required config file, so this content check is the only reliable
// disambiguator from golang-migrate / sqlx-cli / Atlas.
func hasDBmateEvidence(repoRoot string) bool {
	matches, _ := filepath.Glob(filepath.Join(repoRoot, "db", "migrations", "*.sql"))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "-- migrate:up") {
			return true
		}
	}
	return false
}

// hasEFCoreEvidence walks the root and one level deep for a *.csproj
// that references Microsoft.EntityFrameworkCore. EF Core has no
// canonical config file — the csproj content is the only static
// signal — so the Spec uses a permissive marker and leans on this.
func hasEFCoreEvidence(repoRoot string) bool {
	candidates, _ := filepath.Glob(filepath.Join(repoRoot, "*.csproj"))
	nested, _ := filepath.Glob(filepath.Join(repoRoot, "*", "*.csproj"))
	candidates = append(candidates, nested...)
	srcNested, _ := filepath.Glob(filepath.Join(repoRoot, "src", "*", "*.csproj"))
	candidates = append(candidates, srcNested...)
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "Microsoft.EntityFrameworkCore") {
			return true
		}
	}
	return false
}

// gooseEnv scaffolds Goose's native env-var pair. The user edits the
// scheme / credentials; treeman substitutes {target_db} per run.
func gooseEnv() map[string]string {
	return map[string]string{
		"GOOSE_DRIVER":   "postgres",
		"GOOSE_DBSTRING": "postgres://user:password@127.0.0.1:5432/{target_db}?sslmode=disable",
	}
}

// liquibaseEnv scaffolds Liquibase's native LIQUIBASE_COMMAND_* env
// triplet. The CLI reads each of these without a properties file.
func liquibaseEnv() map[string]string {
	return map[string]string{
		"LIQUIBASE_COMMAND_URL":      "jdbc:postgresql://127.0.0.1:5432/{target_db}",
		"LIQUIBASE_COMMAND_USERNAME": "user",
		"LIQUIBASE_COMMAND_PASSWORD": "password",
	}
}

// flywayURLEnv scaffolds FLYWAY_URL with a JDBC connection-string
// template. Flyway's CLI reads FLYWAY_URL natively, so this avoids
// the user having to thread anything through the migrate command.
func flywayURLEnv() map[string]string {
	return map[string]string{
		"FLYWAY_URL": "jdbc:postgresql://127.0.0.1:5432/{target_db}",
	}
}

func builtins() []Spec {
	specs := builtinsClassic()
	specs = append(specs, builtinsJSAndOther()...)
	specs = append(specs, builtinsExtra()...)
	return specs
}

func builtinsClassic() []Spec {
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
			OnModify:      OnRebuild,
			MigrateRun:    "bin/rails db:migrate",
			MigrateEnv:    dbNameEnv(),
		},
		{
			Name:          "django",
			Markers:       []string{"manage.py"},
			MigrationDirs: []string{"**/migrations"},
			FileGlobs:     []string{"[0-9]*_*.py"},
			Lockfiles:     []string{"Pipfile.lock", "poetry.lock", "uv.lock", "requirements.txt"},
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
			OnModify:      OnRebuild,
			// Bare `migrate up` is non-functional — the CLI requires
			// -path/-source and -database flags or env. Scaffold a
			// working form that picks up DATABASE_URL from the env
			// treeman injects via MigrateEnv.
			MigrateRun: `migrate -path migrations -database "$DATABASE_URL" up`,
			MigrateEnv: dbURLEnv(),
			// go.mod alone matches every Go repo, so require a second
			// signal: either the golang-migrate module is imported, or
			// the repo contains at least one *.up.sql file (the
			// framework's distinctive naming convention).
			Validate: hasGolangMigrateEvidence,
		},
	}
}

func builtinsJSAndOther() []Spec {
	return []Spec{
		{
			Name:          "sqlx-cli",
			Markers:       []string{"Cargo.toml", "migrations"},
			MigrationDirs: []string{"migrations", "crates/*/migrations", "services/*/migrations"},
			FileGlobs:     []string{"*.sql"},
			Lockfiles:     []string{"Cargo.lock"},
			OnModify:      OnDelta,
			MigrateRun:    "sqlx migrate run",
			MigrateEnv:    dbURLEnv(),
		},
		{
			Name:          "diesel",
			Markers:       []string{"diesel.toml"},
			MigrationDirs: []string{"migrations", "crates/*/migrations"},
			FileGlobs:     []string{"up.sql"},
			Lockfiles:     []string{"Cargo.lock"},
			OnModify:      OnRebuild,
			MigrateRun:    "diesel migration run",
			MigrateEnv:    dbURLEnv(),
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
			OnModify:   OnDelta,
			MigrateRun: "npx prisma migrate deploy",
			MigrateEnv: dbURLEnv(),
		},
		{
			Name:          "knex",
			Markers:       []string{"knexfile.js|knexfile.ts|knexfile.cjs|knexfile.mjs"},
			MigrationDirs: []string{"migrations", "apps/*/migrations", "packages/*/migrations"},
			FileGlobs:     []string{"*.js", "*.ts", "*.cjs", "*.mjs"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
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
			OnModify:      OnRebuild,
			MigrateRun:    "alembic upgrade head",
			MigrateEnv:    dbNameEnv(),
		},
		{
			Name:          "flyway",
			Markers:       []string{"flyway.conf|flyway.toml"},
			MigrationDirs: []string{"**/db/migration", "sql"},
			FileGlobs:     []string{"[VRU]*.sql"},
			OnModify:      OnRebuild,
			MigrateRun:    "flyway migrate",
			MigrateEnv:    flywayURLEnv(),
		},
		{
			Name: "typeorm",
			Markers: []string{
				"ormconfig.json|ormconfig.js|ormconfig.ts|ormconfig.yaml|ormconfig.yml|data-source.ts|data-source.js|typeorm.config.ts|typeorm.config.js",
			},
			MigrationDirs: []string{
				"src/migrations",
				"migrations",
				"apps/*/src/migrations",
				"packages/*/src/migrations",
			},
			FileGlobs: []string{"*.ts", "*.js"},
			Lockfiles: []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			OnModify:  OnRebuild,
			// v0.3+ requires -d <data-source>; bare `migration:run`
			// fails. typeorm-ts-node-commonjs handles .ts sources
			// without a separate compile step.
			MigrateRun: "npx typeorm-ts-node-commonjs migration:run -- -d ./src/data-source.ts",
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
			OnModify:      OnDelta,
			MigrateRun:    "npx drizzle-kit migrate",
			MigrateEnv:    dbNameEnv(),
		},
	}
}

func builtinsExtra() []Spec {
	return []Spec{
		{
			Name:          "sequelize",
			Markers:       []string{".sequelizerc|.sequelizerc.js|.sequelizerc.cjs"},
			MigrationDirs: []string{"migrations", "apps/*/migrations", "packages/*/migrations"},
			FileGlobs:     []string{"*.js", "*.ts"},
			Lockfiles:     []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			OnModify:      OnRebuild,
			MigrateRun:    "npx sequelize-cli db:migrate",
			MigrateEnv:    dbNameEnv(),
		},
		{
			Name:    "mikro-orm",
			Markers: []string{"mikro-orm.config.ts|mikro-orm.config.js|mikro-orm.config.cjs"},
			MigrationDirs: []string{
				"migrations",
				"src/migrations",
				"apps/*/migrations",
				"apps/*/src/migrations",
				"packages/*/migrations",
				"packages/*/src/migrations",
			},
			FileGlobs:  []string{"Migration*.ts", "Migration*.js"},
			Lockfiles:  []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock"},
			OnModify:   OnRebuild,
			MigrateRun: "npx mikro-orm migration:up",
			MigrateEnv: dbNameEnv(),
		},
		{
			// Symfony bundle (the dominant Doctrine Migrations layout).
			// The standalone library uses different config files and a
			// vendor/bin/doctrine-migrations binary; users on that
			// layout will need to edit the scaffolded Run command.
			Name:          "doctrine-migrations",
			Markers:       []string{"composer.json"},
			MigrationDirs: []string{"migrations", "src/Migrations"},
			FileGlobs:     []string{"Version[0-9]*.php"},
			Lockfiles:     []string{"composer.lock"},
			OnModify:      OnRebuild,
			MigrateRun:    "php bin/console doctrine:migrations:migrate --no-interaction --all-or-nothing",
			MigrateEnv:    dbURLEnv(),
			Validate:      hasDoctrineMigrationsDep,
		},
		{
			// Shares go.mod with golang-migrate; the Validator (go.sum
			// containing pressly/goose) disambiguates. Goose puts both
			// Up and Down in a single annotated .sql file, so the file
			// glob does not collide with golang-migrate's *.up.sql.
			Name:          "goose",
			Markers:       []string{"go.mod"},
			MigrationDirs: []string{"migrations", "db/migrations", "internal/db/migrations", "cmd/*/migrations"},
			FileGlobs:     []string{"[0-9]*_*.sql", "[0-9]*_*.go"},
			Lockfiles:     []string{"go.sum"},
			OnModify:      OnRebuild,
			MigrateRun:    "goose -dir migrations up",
			MigrateEnv:    gooseEnv(),
			Validate:      hasGooseEvidence,
		},
		{
			Name:          "liquibase",
			Markers:       []string{"liquibase.properties|liquibase.yaml|liquibase.yml|liquibase.json"},
			MigrationDirs: []string{"db/changelog", "src/main/resources/db/changelog", "changelog", "changelogs"},
			FileGlobs:     []string{"*.xml", "*.yaml", "*.yml", "*.json", "*.sql"},
			OnModify:      OnRebuild,
			MigrateRun:    "liquibase --changelog-file=db.changelog-master.xml update",
			MigrateEnv:    liquibaseEnv(),
		},
		{
			// EF Core has no canonical project-root marker file — only
			// a *.csproj that references Microsoft.EntityFrameworkCore.
			// Use the repo-root sentinel "." so Detect always defers
			// to the Validator, which walks for the csproj content.
			Name:          "ef-core",
			Markers:       []string{"."},
			MigrationDirs: []string{"Migrations", "*/Migrations", "src/*/Migrations"},
			FileGlobs:     []string{"[0-9]*_*.cs"},
			Lockfiles:     []string{"packages.lock.json", "global.json"},
			OnModify:      OnRebuild,
			MigrateRun:    "dotnet ef database update",
			MigrateEnv:    dbNameEnv(),
			Validate:      hasEFCoreEvidence,
		},
		{
			// mix.exs matches every Elixir project, so the Validator
			// (ecto_sql in mix.lock OR a default priv/repo/migrations
			// dir) is the real gate.
			Name:          "ecto",
			Markers:       []string{"mix.exs"},
			MigrationDirs: []string{"priv/repo/migrations", "apps/*/priv/*/migrations"},
			FileGlobs:     []string{"[0-9]*_*.exs"},
			Lockfiles:     []string{"mix.lock"},
			OnModify:      OnRebuild,
			MigrateRun:    "mix ecto.migrate",
			MigrateEnv:    dbNameEnv(),
			Validate:      hasEctoEvidence,
		},
		{
			// DBmate ships no required config file; the Validator
			// scans db/migrations for the distinctive `-- migrate:up`
			// directive.
			Name:          "dbmate",
			Markers:       []string{"db/migrations"},
			MigrationDirs: []string{"db/migrations"},
			FileGlobs:     []string{"[0-9]*_*.sql"},
			OnModify:      OnRebuild,
			MigrateRun:    "dbmate up",
			MigrateEnv:    dbURLEnv(),
			Validate:      hasDBmateEvidence,
		},
		{
			// atlas.hcl is the strong, distinctive marker; the fallback
			// migrations/atlas.sum covers users who scaffolded via
			// `atlas migrate diff` without authoring an HCL config.
			Name:          "atlas",
			Markers:       []string{"atlas.hcl|migrations/atlas.sum"},
			MigrationDirs: []string{"migrations"},
			FileGlobs:     []string{"[0-9]*_*.sql"},
			OnModify:      OnDelta,
			MigrateRun:    `atlas migrate apply --url "$DATABASE_URL" --dir "file://migrations"`,
			MigrateEnv:    dbURLEnv(),
		},
	}
}
