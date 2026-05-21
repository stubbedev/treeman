package resolve

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadResolvedFillsCredsFromEnvWithoutYaml is the regression
// for the original bug: a `.treeman.yaml` that omits the
// `connections:` block should still get MySQL creds populated from
// `.env.testing` (Laravel-style `DB_*`).
func TestLoadResolvedFillsCredsFromEnvWithoutYaml(t *testing.T) {
	// Isolate from the developer's global config.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	repo := t.TempDir()
	// Minimal .treeman.yaml — NO connections: block.
	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(`
repo:
  name: laravel-app
databases:
  - engine: mysql
    name_template: "app_testing_{slug}"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.testing"), []byte(`
DB_HOST=10.0.0.42
DB_PORT=3307
DB_USERNAME=alice
DB_PASSWORD=hunter2
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadResolved(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connections.Mysql == nil {
		t.Fatal("expected connections.mysql to be populated from .env.testing")
	}
	got := cfg.Connections.Mysql
	if got.Host != "10.0.0.42" {
		t.Errorf("host=%q want 10.0.0.42", got.Host)
	}
	if got.Port != 3307 {
		t.Errorf("port=%d want 3307", got.Port)
	}
	if got.User != "alice" {
		t.Errorf("user=%q want alice", got.User)
	}
	if got.Password != "hunter2" {
		t.Errorf("password not picked up from DB_PASSWORD")
	}
}

// TestEnvScopingSourcesOrderingLastWins exercises the configurable
// env-source list: when the YAML names a sequence, later entries
// override earlier ones — even when the default order would say
// otherwise.
func TestEnvScopingSourcesOrderingLastWins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()

	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(`
repo:
  name: app
env_scoping:
  sources:
    - .env.base
    - .env.override
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.base"), []byte("DB_HOST=base\nDB_PASSWORD=base-pw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.override"), []byte("DB_PASSWORD=winner\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadResolved(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connections.Mysql == nil {
		t.Fatal("expected mysql conn populated")
	}
	if cfg.Connections.Mysql.Host != "base" {
		t.Errorf("host=%q want base (only present in base)", cfg.Connections.Mysql.Host)
	}
	if cfg.Connections.Mysql.Password != "winner" {
		t.Errorf("password=%q want winner (override layer wins)", cfg.Connections.Mysql.Password)
	}
}

// TestLoadResolvedYamlPlusEnvPassword covers the common Laravel
// case: YAML pins host/port/user, .env.testing supplies the
// password.
func TestLoadResolvedYamlPlusEnvPassword(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(`
repo:
  name: app
connections:
  mysql:
    host: 127.0.0.1
    port: 3306
    user: root
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.testing"), []byte("DB_PASSWORD=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadResolved(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connections.Mysql.Password != "secret" {
		t.Errorf("password=%q want secret", cfg.Connections.Mysql.Password)
	}
	if cfg.Connections.Mysql.Host != "127.0.0.1" {
		t.Errorf("host=%q want 127.0.0.1", cfg.Connections.Mysql.Host)
	}
}
