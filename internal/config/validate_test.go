package config

import (
	"strings"
	"testing"
)

// Shared pointers for DatabaseConfig.PhysicalCloneMinBytes cases —
// the field is *int64 so config can distinguish "unset" from 0
// ("always attempt the physical clone path").
var (
	zeroInt64 = int64(0)
	someInt64 = int64(1 << 20)
	negInt64  = int64(-1)
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string // substring of error; empty = no error
	}{
		{
			name: "empty config is valid",
			cfg:  Config{},
			want: "",
		},
		{
			name: "container and compose_service mutually exclusive",
			cfg: Config{
				Connections: ConnectionsConfig{
					Mysql: &MysqlConn{
						Container:      "db",
						ComposeService: "mysql",
					},
				},
			},
			want: "mutually exclusive",
		},
		{
			name: "mysql charset identifier-like is valid",
			cfg: Config{
				Connections: ConnectionsConfig{
					Mysql: &MysqlConn{Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci"},
				},
			},
			want: "",
		},
		{
			name: "mysql charset with SQL metacharacters rejected",
			cfg: Config{
				Connections: ConnectionsConfig{
					Mysql: &MysqlConn{Charset: "utf8mb4; DROP DATABASE x"},
				},
			},
			want: "connections.mysql.charset",
		},
		{
			name: "mysql collation with quote rejected",
			cfg: Config{
				Connections: ConnectionsConfig{
					Mysql: &MysqlConn{Collation: "utf8'x"},
				},
			},
			want: "connections.mysql.collation",
		},
		{
			name: "redis db window within 0..15 is valid",
			cfg: Config{
				Connections: ConnectionsConfig{
					Redis: &RedisConn{URL: "redis://localhost:6379/0", DBMin: new(0), DBMax: new(0)},
				},
			},
			want: "",
		},
		{
			name: "redis db_min above 15 rejected",
			cfg: Config{
				Connections: ConnectionsConfig{
					Redis: &RedisConn{URL: "redis://localhost:6379/0", DBMin: new(16)},
				},
			},
			want: "db_min: 16 outside",
		},
		{
			name: "redis db_min above db_max rejected",
			cfg: Config{
				Connections: ConnectionsConfig{
					Redis: &RedisConn{URL: "redis://localhost:6379/0", DBMin: new(9), DBMax: new(3)},
				},
			},
			want: "db_min 9 > db_max 3",
		},
		{
			name: "database engine required",
			cfg: Config{
				Databases: []DatabaseConfig{{NameTemplate: "x_{slug}"}},
			},
			want: "engine is required",
		},
		{
			name: "database engine unknown",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "sqlite3", NameTemplate: "x_{slug}"}},
			},
			want: "unknown engine",
		},
		{
			name: "name_template required for mysql",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "mysql"}},
			},
			want: "name_template is required",
		},
		{
			name: "redis without name_template ok",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "redis", KeyPrefix: "app:{slug}:"}},
			},
			want: "",
		},
		{
			name: "elasticsearch without name_template ok",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "elasticsearch", KeyPrefix: "app_{slug}_"}},
			},
			want: "",
		},
		{
			name: "redis without key_prefix rejected",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "redis"}},
			},
			want: "key_prefix is required for engine \"redis\"",
		},
		{
			name: "name_template without a slug key rejected",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "mysql", NameTemplate: "app_testing"}},
			},
			want: "must contain {slug} or {slug_dash}",
		},
		{
			name: "key_prefix without a slug key rejected",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "elasticsearch", KeyPrefix: "app_"}},
			},
			want: "must contain {slug} or {slug_dash}",
		},
		{
			name: "slug_dash satisfies the slug requirement",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "s3", KeyPrefix: "myapp-{slug_dash}"}},
			},
			want: "",
		},
		{
			name: "test_clones template without {n} rejected",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
					TestClones:   &TestClonesSpec{Clones: ClonesSetting{Fixed: 2}, NameTemplate: "app_{slug}_test"},
				}},
			},
			want: "must contain {n} exactly once",
		},
		{
			name: "main_worktree overlay may bind a bare name",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
					TestClones:   &TestClonesSpec{Clones: ClonesSetting{Fixed: 2}, NameTemplate: "app_{slug}_test_{n}"},
				}},
				MainWorktree: MainWorktreeConfig{Databases: []DatabaseOverlay{{
					NameTemplate: "app",
					TestClones:   &TestClonesSpec{Clones: ClonesSetting{Fixed: 2}, NameTemplate: "app_test_{n}"},
				}}},
			},
			want: "",
		},
		{
			name: "valid config is valid",
			cfg: Config{
				Databases: []DatabaseConfig{
					{Engine: "mysql", NameTemplate: "app_{slug}"},
					{Engine: "redis", KeyPrefix: "app:{slug}:"},
				},
			},
			want: "",
		},
		{
			name: "port range min greater than max",
			cfg: Config{
				Ports: map[string]PortSpec{"octane": {Range: PortRange{Min: 9000, Max: 8000}}},
			},
			want: "range invalid",
		},
		{
			name: "port range zero",
			cfg: Config{
				Ports: map[string]PortSpec{"octane": {Range: PortRange{Min: 0, Max: 0}}},
			},
			want: "must be non-zero",
		},
		{
			name: "patch may reference declared port slot",
			cfg: Config{
				Ports: map[string]PortSpec{"octane": {Range: PortRange{Min: 8000, Max: 8999}}},
				Patches: []Patch{{
					File: ".env",
					Set:  map[string]string{"OCTANE_PORT": "{port_octane}"},
				}},
			},
			want: "",
		},
		{
			name: "patch referencing undeclared port slot fails",
			cfg: Config{
				Ports: map[string]PortSpec{"octane": {Range: PortRange{Min: 8000, Max: 8999}}},
				Patches: []Patch{{
					File: ".env",
					Set:  map[string]string{"WEBPACK_PORT": "{port_webpack}"},
				}},
			},
			want: "unknown template key: port_webpack",
		},
		{
			name: "patch port token without any ports declared fails",
			cfg: Config{
				Patches: []Patch{{
					File: ".env",
					Set:  map[string]string{"OCTANE_PORT": "{port_octane}"},
				}},
			},
			want: "unknown template key: port_octane",
		},
		{
			name: "branch_scoped and test_clones are mutually exclusive",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
					BranchScoped: true,
					TestClones:   &TestClonesSpec{NameTemplate: "app_{slug}_test_{n}"},
				}},
			},
			want: "branch_scoped and test_clones are mutually exclusive",
		},
		{
			name: "branch_scoped rejects fanout",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "postgres",
					NameTemplate: "app_{slug}",
					BranchScoped: true,
					Fanout:       8,
				}},
			},
			want: "branch_scoped databases do not fan out",
		},
		{
			name: "branch_scoped alone is valid",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
					BranchScoped: true,
				}},
			},
			want: "",
		},
		{
			name: "prewarm on postgres is valid",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "postgres",
					NameTemplate: "app_{slug}",
					Prewarm:      2,
				}},
			},
			want: "",
		},
		{
			name: "prewarm accepts capability-bearing engines",
			cfg: Config{
				Databases: []DatabaseConfig{
					{Engine: "mysql", NameTemplate: "app_{slug}", Prewarm: 2},
					{Engine: "postgres", NameTemplate: "app_{slug}", Prewarm: 2},
				},
			},
			want: "",
		},
		{
			name: "prewarm rejects engines without the spare capability",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:    "redis",
					KeyPrefix: "app:{slug}:",
					Prewarm:   2,
				}},
			},
			want: "prewarm is not supported",
		},
		{
			name: "prewarm rejects branch_scoped",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "postgres",
					NameTemplate: "app_{slug}",
					BranchScoped: true,
					Prewarm:      1,
				}},
			},
			want: "branch_scoped and prewarm are mutually exclusive",
		},
		{
			name: "physical_clone_min_bytes on mysql is valid",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:                "mysql",
					NameTemplate:          "app_{slug}",
					PhysicalCloneMinBytes: &zeroInt64,
				}},
			},
			want: "",
		},
		{
			name: "physical_clone_min_bytes accepts an alias engine",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:                "mariadb",
					NameTemplate:          "app_{slug}",
					PhysicalCloneMinBytes: &someInt64,
				}},
			},
			want: "",
		},
		{
			name: "physical_clone_min_bytes rejects non-mysql engines",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:                "postgres",
					NameTemplate:          "app_{slug}",
					PhysicalCloneMinBytes: &someInt64,
				}},
			},
			want: "physical_clone_min_bytes is mysql-only",
		},
		{
			name: "physical_clone_min_bytes rejects negatives",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:                "mysql",
					NameTemplate:          "app_{slug}",
					PhysicalCloneMinBytes: &negInt64,
				}},
			},
			want: "physical_clone_min_bytes must be >= 0",
		},
		{
			name: "branch_scoped + main_worktree.enabled with no overlay rejects slug-bearing main name",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
					BranchScoped: true,
				}},
				MainWorktree: MainWorktreeConfig{Enabled: true},
			},
			want: "requires a slug-free main_worktree.databases[0].name_template",
		},
		{
			name: "branch_scoped + main_worktree.enabled with slug-bearing overlay still rejected",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
					BranchScoped: true,
				}},
				MainWorktree: MainWorktreeConfig{
					Enabled:   true,
					Databases: []DatabaseOverlay{{NameTemplate: "app_{slug}_main"}},
				},
			},
			want: "slug-free",
		},
		{
			name: "branch_scoped + main_worktree.enabled with bare overlay is valid",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
					BranchScoped: true,
				}},
				MainWorktree: MainWorktreeConfig{
					Enabled:   true,
					Databases: []DatabaseOverlay{{NameTemplate: "app"}},
				},
			},
			want: "",
		},
		{
			name: "branch_scoped redis + main_worktree.enabled requires slug-free key_prefix",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "redis",
					KeyPrefix:    "{slug}:",
					BranchScoped: true,
				}},
				MainWorktree: MainWorktreeConfig{Enabled: true},
			},
			want: "key_prefix",
		},
		{
			name: "s3 missing key_prefix",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "s3"}},
			},
			want: "key_prefix is required for engine \"s3\"",
		},
		{
			name: "s3 key_prefix literal too short",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "s3", KeyPrefix: "dev-{slug}"}},
			},
			want: "literal portion",
		},
		{
			name: "s3 rejects dump",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:    "s3",
					KeyPrefix: "myapp-{slug}",
					Dump:      DumpList{{Path: "dump.sql"}},
				}},
			},
			want: "engine \"s3\" does not support `dump:`",
		},
		{
			name: "s3 accepts branch_scoped",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "s3",
					KeyPrefix:    "myapp-{slug}",
					BranchScoped: true,
				}},
			},
			want: "", // branch_scoped is supported (per-branch durable bucket copy)
		},
		{
			name: "s3 rejects test_clones",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:     "s3",
					KeyPrefix:  "myapp-{slug}",
					TestClones: &TestClonesSpec{NameTemplate: "x"},
				}},
			},
			want: "engine \"s3\" does not support `test_clones:`",
		},
		{
			name: "s3 rejects migrate",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:    "s3",
					KeyPrefix: "myapp-{slug}",
					Migrate:   &Step{Run: "./mig.sh"},
				}},
			},
			want: "engine \"s3\" does not support `migrate:`",
		},
		{
			name: "s3 rejects seed",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:    "s3",
					KeyPrefix: "myapp-{slug}",
					Seed:      &Step{Run: "./seed.sh"},
				}},
			},
			want: "engine \"s3\" does not support `seed:`",
		},
		{
			name: "s3 with long literal prefix is valid",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:    "s3",
					KeyPrefix: "myapp-{slug}",
				}},
			},
			want: "",
		},
		{
			name: "non-branch_scoped slug name on main worktree is fine",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
				}},
				MainWorktree: MainWorktreeConfig{Enabled: true},
			},
			want: "",
		},
		{
			name: "step env may reference a sibling database by index",
			cfg: Config{
				Databases: []DatabaseConfig{
					{
						Engine:       "mysql",
						NameTemplate: "app_testing_{slug}",
						Migrate:      &Step{Run: "./mig.sh", Env: map[string]string{"MONGO_DB_DATABASE": "{db:1}"}},
						Rollback:     &Step{Run: "./rb.sh", Env: map[string]string{"MONGO_DB_DATABASE": "{db:1}"}},
						Seed:         &Step{Run: "./seed.sh", Env: map[string]string{"MONGO_DB_DATABASE": "{db:1}"}},
					},
					{Engine: "mongodb", NameTemplate: "mongodb_testing_{slug}"},
				},
			},
			want: "",
		},
		{
			name: "step env db index past the declared databases",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "mysql",
					NameTemplate: "app_{slug}",
					Migrate:      &Step{Run: "./mig.sh", Env: map[string]string{"MONGO_DB_DATABASE": "{db:1}"}},
				}},
			},
			want: "databases[0].migrate.env[MONGO_DB_DATABASE]: unknown template key: db:1",
		},
		{
			name: "db token outside step env",
			cfg: Config{
				Databases: []DatabaseConfig{
					{Engine: "mysql", NameTemplate: "app_{slug}_{db:1}"},
					{Engine: "mysql", NameTemplate: "other_{slug}"},
				},
			},
			want: "databases[0].name_template: unknown template key: db:1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q should contain %q", err, c.want)
			}
		})
	}
}

// TestValidateFileFamily locks the file-backed engines' (sqlite /
// duckdb) constraints: name_template IS the database file path, so it
// must be relative and stay inside the worktree (or base_dir);
// key_prefix and branch_scoped have no file-family meaning; and a
// duckdb entry can't execute .sql dumps from Go.
func TestValidateFileFamily(t *testing.T) {
	cases := []struct {
		name string
		d    DatabaseConfig
		want string
	}{
		{
			name: "sqlite with relative path ok",
			d:    DatabaseConfig{Engine: "sqlite", NameTemplate: "data/{slug}.db"},
			want: "",
		},
		{
			name: "duckdb with base-file dump ok",
			d: DatabaseConfig{
				Engine:       "duckdb",
				NameTemplate: "warehouse/{slug}.duckdb",
				Dump:         DumpList{{Path: "fixtures/seed.duckdb"}},
			},
			want: "",
		},
		{
			name: "sqlite with .sql dump ok",
			d: DatabaseConfig{
				Engine:       "sqlite",
				NameTemplate: "data/{slug}.db",
				Dump:         DumpList{{Path: "fixtures/schema.sql"}},
			},
			want: "",
		},
		{
			name: "absolute name_template rejected",
			d:    DatabaseConfig{Engine: "sqlite", NameTemplate: "/tmp/app.db"},
			want: "relative path",
		},
		{
			name: "dotdot escape rejected",
			d:    DatabaseConfig{Engine: "sqlite", NameTemplate: "data/../../escape.db"},
			want: `escape`,
		},
		{
			name: "key_prefix rejected",
			d:    DatabaseConfig{Engine: "sqlite", NameTemplate: "data/{slug}.db", KeyPrefix: "app:{slug}:"},
			want: "key_prefix",
		},
		{
			name: "branch_scoped rejected",
			d:    DatabaseConfig{Engine: "sqlite", NameTemplate: "data/{slug}.db", BranchScoped: true},
			want: "branch_scoped",
		},
		{
			name: "duckdb .sql dump rejected",
			d: DatabaseConfig{
				Engine:       "duckdb",
				NameTemplate: "warehouse/{slug}.duckdb",
				Dump:         DumpList{{Path: "fixtures/schema.sql"}},
			},
			want: ".sql",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.d.validate("databases[0]", 1)
			if c.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q should contain %q", err, c.want)
			}
		})
	}
}
