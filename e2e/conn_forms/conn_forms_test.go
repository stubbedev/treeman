//go:build e2e

// Package connforms_e2e exercises the bare-scalar connection forms the
// schema documents via oneOf(string|object): mysql/postgres DSN strings
// and the mongo/redis/es URI/URL strings. Every other e2e builds
// config.Config structs directly, so the UnmarshalYAML + parseDSN paths
// were never hit end-to-end. This loads a real .treeman.yaml with scalar
// connections and runs prepare against real servers.
package connforms_e2e

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
)

const (
	mysqlAddr = "127.0.0.1:13344"
	pgAddr    = "127.0.0.1:15434"
	redisAddr = "127.0.0.1:16380"
	mongoAddr = "127.0.0.1:27027"
	esAddr    = "127.0.0.1:9244"
)

func TestConnectionScalarForms(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate the global config layer
	waitTCP(t, "mysql", mysqlAddr)
	waitTCP(t, "postgres", pgAddr)
	waitTCP(t, "redis", redisAddr)
	waitTCP(t, "mongo", mongoAddr)
	waitHTTP(t, "elasticsearch", "http://"+esAddr+"/_cluster/health")

	t.Run("mysql DSN string", func(t *testing.T) {
		wt := setupRepo(t, `connections:
  mysql: "mysql://root:rootpw@127.0.0.1:13344/"
databases:
  - engine: mysql
    name_template: tmcf_my_{slug}
    dump: seed.sql
`)
		write(t, wt, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1);")
		o := prepareLoaded(t, wt, "mysql")
		if !mysqlDBExists(t, o.SourceDB) {
			t.Errorf("mysql DSN-string form: source DB %s not created", o.SourceDB)
		}
	})

	t.Run("postgres DSN string", func(t *testing.T) {
		wt := setupRepo(t, `connections:
  postgres: "postgres://postgres:pgpw@127.0.0.1:15434/postgres?sslmode=disable"
databases:
  - engine: postgres
    name_template: tmcf_pg_{slug}
    dump: seed.sql
`)
		write(t, wt, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1);")
		o := prepareLoaded(t, wt, "postgres")
		if !pgDBExists(t, o.SourceDB) {
			t.Errorf("postgres DSN-string form: source DB %s not created", o.SourceDB)
		}
	})

	t.Run("redis URL string", func(t *testing.T) {
		wt := setupRepo(t, `connections:
  redis: "redis://127.0.0.1:16380"
databases:
  - engine: redis
    key_prefix: "tmcf_rd_{slug}:"
`)
		// A successful prepare proves the scalar URL parsed + connected:
		// prepareRedis dials redis (PrefixExists) before any cold build,
		// so a bad URL would fail RunPrepare.
		o := prepareLoaded(t, wt, "redis")
		if o.Engine != "redis" {
			t.Errorf("redis URL-string form: unexpected outcome engine %q", o.Engine)
		}
	})

	t.Run("mongo URI string", func(t *testing.T) {
		wt := setupRepo(t, `connections:
  mongodb: "mongodb://127.0.0.1:27027"
databases:
  - engine: mongodb
    name_template: tmcf_mo_{slug}
`)
		// A successful prepare proves the scalar URI parsed + dialed:
		// prepareMongo connects (ListMatching) before snapshot, so a bad
		// URI would fail RunPrepare.
		o := prepareLoaded(t, wt, "mongodb")
		if o.Engine != "mongodb" {
			t.Errorf("mongo URI-string form: unexpected outcome engine %q", o.Engine)
		}
	})

	t.Run("elasticsearch URL string", func(t *testing.T) {
		wt := setupRepo(t, `connections:
  elasticsearch: "http://127.0.0.1:9244"
databases:
  - engine: elasticsearch
    key_prefix: "tmcf_es_{slug}_"
`)
		// A successful prepare proves the scalar URL parsed + reached the
		// cluster: prepare probes ES (ListMatching) before any build.
		o := prepareLoaded(t, wt, "elasticsearch")
		if o.Engine != "elasticsearch" {
			t.Errorf("es URL-string form: unexpected outcome engine %q", o.Engine)
		}
	})
}

// ─── helpers ─────────────────────────────────────────────────────────

// setupRepo writes a worktree dir with the given .treeman.yaml and
// returns its path.
func setupRepo(t *testing.T, yaml string) string {
	t.Helper()
	wt := t.TempDir()
	write(t, wt, ".treeman.yaml", yaml)
	return wt
}

// prepareLoaded loads the config from the worktree's .treeman.yaml (so
// the scalar connection form goes through UnmarshalYAML/parseDSN) and
// runs prepare, returning the named engine's outcome.
func prepareLoaded(t *testing.T, wt, engine string) prepare.Outcome {
	t.Helper()
	cfg, err := config.LoadLayered(wt)
	if err != nil {
		t.Fatalf("LoadLayered (scalar conn form did not parse?): %v", err)
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, &cfg), engine, false)
	return o
}

func mysqlDBExists(t *testing.T, name string) bool {
	t.Helper()
	db, _ := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
	defer db.Close()
	var n int
	_ = db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&n)
	return n == 1
}

func pgDBExists(t *testing.T, name string) bool {
	t.Helper()
	db, _ := sql.Open("pgx", "postgres://postgres:pgpw@"+pgAddr+"/postgres?sslmode=disable")
	defer db.Close()
	var n int
	_ = db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n)
	return n == 1
}

func waitTCP(t *testing.T, name, addr string) {
	t.Helper()
	harness.WaitForReady(t, name+":"+addr, 60*time.Second, func() error {
		switch name {
		case "mysql":
			db, err := sql.Open("mysql", "root:rootpw@tcp("+addr+")/")
			if err != nil {
				return err
			}
			defer db.Close()
			return db.Ping()
		case "postgres":
			db, err := sql.Open("pgx", "postgres://postgres:pgpw@"+addr+"/postgres?sslmode=disable")
			if err != nil {
				return err
			}
			defer db.Close()
			return db.Ping()
		default:
			c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
			if err != nil {
				return err
			}
			_ = c.Close()
			return nil
		}
	})
}

func waitHTTP(t *testing.T, name, url string) {
	t.Helper()
	harness.WaitForReady(t, name+":"+url, 90*time.Second, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			return mkHTTPErr(resp.StatusCode)
		}
		return nil
	})
}

func mkHTTPErr(code int) error { return &httpErr{code} }

type httpErr struct{ code int }

func (e *httpErr) Error() string { return "http status " + strconv.Itoa(e.code) }

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
