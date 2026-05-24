package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConnectionsAcceptDSNStrings(t *testing.T) {
	y := `
connections:
  mysql: "mysql://root:secret@10.0.0.5:3307"
  postgres: "postgres://postgres:pgpw@127.0.0.1:5432"
  mongodb: "mongodb://127.0.0.1:27017"
  redis: "redis://127.0.0.1:6379"
  elasticsearch: "http://127.0.0.1:9200"
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(y), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Connections.Mysql == nil {
		t.Fatal("mysql nil")
	}
	if cfg.Connections.Mysql.Host != "10.0.0.5" {
		t.Errorf("mysql host = %q", cfg.Connections.Mysql.Host)
	}
	if cfg.Connections.Mysql.Port != 3307 {
		t.Errorf("mysql port = %d", cfg.Connections.Mysql.Port)
	}
	if cfg.Connections.Mysql.User != "root" {
		t.Errorf("mysql user = %q", cfg.Connections.Mysql.User)
	}
	if cfg.Connections.Mysql.Password != "secret" {
		t.Errorf("mysql password = %q", cfg.Connections.Mysql.Password)
	}
	if cfg.Connections.Postgres == nil {
		t.Fatal("postgres nil")
	}
	if cfg.Connections.Postgres.User != "postgres" {
		t.Errorf("postgres user = %q", cfg.Connections.Postgres.User)
	}
	if cfg.Connections.Postgres.Port != 5432 {
		t.Errorf("postgres port = %d", cfg.Connections.Postgres.Port)
	}
}

func TestStructuredFormStillWorks(t *testing.T) {
	y := `
connections:
  mysql:
    host: db.internal
    port: 3306
    user: app
    password: $APP_DB_PASS
    pool_max: 32
  postgres:
    host: 10.0.0.1
    user: postgres
    password: ${PG_PASS}
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(y), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Connections.Mysql.Host != "db.internal" {
		t.Errorf("mysql host wrong")
	}
	if cfg.Connections.Mysql.PoolMax != 32 {
		t.Errorf("mysql PoolMax = %d", cfg.Connections.Mysql.PoolMax)
	}
	// Pre-resolve, the password field holds the raw `$NAME` ref.
	if cfg.Connections.Mysql.Password != "$APP_DB_PASS" {
		t.Errorf("mysql password literal = %q", cfg.Connections.Mysql.Password)
	}
	if cfg.Connections.Postgres.Password != "${PG_PASS}" {
		t.Errorf("postgres password literal = %q", cfg.Connections.Postgres.Password)
	}
}

func TestDSNRejectsWrongScheme(t *testing.T) {
	y := `
connections:
  mysql: "postgres://x@y"
`
	var cfg Config
	err := yaml.Unmarshal([]byte(y), &cfg)
	if err == nil {
		t.Fatal("want scheme-mismatch error")
	}
	if !strings.Contains(err.Error(), "scheme must be mysql") {
		t.Errorf("unexpected error: %v", err)
	}
}
