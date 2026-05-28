package config

import (
	"strings"
	"testing"
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
						ContainerRef: ContainerRef{
							Container:      "db",
							ComposeService: "mysql",
						},
					},
				},
			},
			want: "mutually exclusive",
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
				Databases: []DatabaseConfig{{Engine: "redis"}},
			},
			want: "",
		},
		{
			name: "elasticsearch without name_template ok",
			cfg: Config{
				Databases: []DatabaseConfig{{Engine: "elasticsearch"}},
			},
			want: "",
		},
		{
			name: "valid config is valid",
			cfg: Config{
				Databases: []DatabaseConfig{
					{Engine: "mysql", NameTemplate: "app_{slug}"},
					{Engine: "redis"},
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
					Dump:      &DumpSpec{Path: "dump.sql"},
				}},
			},
			want: "engine \"s3\" does not support `dump:`",
		},
		{
			name: "s3 rejects branch_scoped",
			cfg: Config{
				Databases: []DatabaseConfig{{
					Engine:       "s3",
					KeyPrefix:    "myapp-{slug}",
					BranchScoped: true,
				}},
			},
			want: "engine \"s3\" does not support `branch_scoped: true`",
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
