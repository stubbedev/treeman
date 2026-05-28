//go:build e2e

// Package enginealiases_e2e exercises the non-canonical `engine:` values
// the schema accepts — mariadb, tidb, postgresql, opensearch — which
// canonicalize to the mysql / postgres / elasticsearch families. The
// per-engine suites only use the canonical names; this proves the alias
// strings are accepted by config + routed to the right driver and
// actually create the namespace against a compatible server.
package enginealiases_e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

const (
	mysqlAddr = "127.0.0.1:13343"
	pgAddr    = "127.0.0.1:15433"
	esURL     = "http://127.0.0.1:19201"
)

func TestEngineAliases(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	waitMySQL(t)
	waitPostgres(t)
	waitES(t)

	// mariadb + tidb → mysql family / mysql driver / mysql server.
	for _, alias := range []string{"mariadb", "tidb"} {
		alias := alias
		t.Run(alias, func(t *testing.T) {
			wt := t.TempDir()
			writeFile(t, wt, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1);")
			cfg := &config.Config{
				Connections: config.ConnectionsConfig{
					Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13343, User: "root", Password: "rootpw"},
				},
				Databases: []config.DatabaseConfig{{
					Engine:       alias,
					NameTemplate: "tmal_" + alias + "_{slug}",
					Dump:         &config.DumpSpec{Path: "seed.sql"},
				}},
			}
			o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), alias, false)
			if !mysqlDBExists(t, o.SourceDB) {
				t.Errorf("alias %s: source DB %s not created", alias, o.SourceDB)
			}
		})
	}

	// postgresql → postgres family.
	t.Run("postgresql", func(t *testing.T) {
		wt := t.TempDir()
		writeFile(t, wt, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1);")
		cfg := &config.Config{
			Connections: config.ConnectionsConfig{
				Postgres: &config.PostgresConn{Host: "127.0.0.1", Port: 15433, User: "postgres", Password: "pgpw"},
			},
			Databases: []config.DatabaseConfig{{
				Engine:       "postgresql",
				NameTemplate: "tmal_pg_{slug}",
				Dump:         &config.DumpSpec{Path: "seed.sql"},
			}},
		}
		o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "postgresql", false)
		if !pgDBExists(t, o.SourceDB) {
			t.Errorf("alias postgresql: source DB %s not created", o.SourceDB)
		}
	})

	// opensearch → elasticsearch family / ES driver.
	t.Run("opensearch", func(t *testing.T) {
		wt := t.TempDir()
		writeFile(t, wt, "dump.ndjson", `{"index":{"_index":"{target_db}items","_id":"1"}}`+"\n"+`{"v":"x"}`+"\n")
		cfg := &config.Config{
			Connections: config.ConnectionsConfig{
				Elasticsearch: &config.EsConn{URL: esURL},
			},
			Databases: []config.DatabaseConfig{{
				Engine:    "opensearch",
				KeyPrefix: "tmal_os_{slug}_",
				Dump:      &config.DumpSpec{Path: "dump.ndjson"},
			}},
		}
		o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "opensearch", false)
		if esCount(t, o.SourceDB+"*") < 1 {
			t.Errorf("alias opensearch: no indices created under %s*", o.SourceDB)
		}
	})
}

// ─── helpers ─────────────────────────────────────────────────────────

func mysqlDBExists(t *testing.T, name string) bool {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	defer db.Close()
	var n int
	_ = db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&n)
	return n == 1
}

func pgDBExists(t *testing.T, name string) bool {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://postgres:pgpw@"+pgAddr+"/postgres?sslmode=disable")
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer db.Close()
	var n int
	_ = db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n)
	return n == 1
}

func esCount(t *testing.T, pattern string) int {
	t.Helper()
	resp, err := http.Get(esURL + "/" + pattern + "/_count")
	if err != nil {
		t.Fatalf("es count: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return -1
	}
	var out struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Count
}

func waitMySQL(t *testing.T) {
	harness.WaitForReady(t, "mysql:"+mysqlAddr, 60*time.Second, func() error {
		db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})
}

func waitPostgres(t *testing.T) {
	harness.WaitForReady(t, "postgres:"+pgAddr, 60*time.Second, func() error {
		db, err := sql.Open("pgx", "postgres://postgres:pgpw@"+pgAddr+"/postgres?sslmode=disable")
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})
}

func waitES(t *testing.T) {
	harness.WaitForReady(t, "es:"+esURL, 120*time.Second, func() error {
		resp, err := http.Get(esURL + "/_cluster/health?wait_for_status=yellow&timeout=5s")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return io.EOF
		}
		return nil
	})
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
