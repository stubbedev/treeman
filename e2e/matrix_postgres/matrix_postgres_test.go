//go:build e2e

// Package matrixpg_e2e fills the postgres cells of the option×engine
// matrix that were previously mysql-only: compose_service resolution,
// test_clones:auto detection, password $ENV-ref resolution, and
// pool_max concurrency capping — all against a real postgres.
package matrixpg_e2e

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/resolve"
)

const pgAddr = "127.0.0.1:15440"

func up(t *testing.T) {
	t.Helper()
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:"+pgAddr, 60*time.Second, func() error {
		db, err := sql.Open("pgx", dsn("postgres"))
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})
}

// ─── compose_service ─────────────────────────────────────────────────

func TestPostgresComposeService(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	project := composeProject(t, "treeman-e2e-mxpg")

	wt := t.TempDir()
	write(t, wt, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1);")
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				User: "postgres", Password: "pgpw",
				ContainerRef: config.ContainerRef{ComposeService: "postgres", ComposeProject: project},
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine: "postgres", NameTemplate: "mxpg_svc_{slug}", Dump: config.DumpList{{Path: "seed.sql"}},
		}},
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "postgres", false)
	if !pgDBExists(t, o.SourceDB) {
		t.Errorf("compose_service: source DB %s not created", o.SourceDB)
	}
}

// ─── test_clones: auto ───────────────────────────────────────────────

func TestPostgresClonesAuto(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	wt := t.TempDir()
	write(t, wt, "package.json", `{"devDependencies":{"jest":"^29"}}`) // per-worker fw → NumCPUs
	write(t, wt, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2);")
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{Host: "127.0.0.1", Port: 15440, User: "postgres", Password: "pgpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine: "postgres", NameTemplate: "mxpg_ca_{slug}", Dump: config.DumpList{{Path: "seed.sql"}},
			TestClones: &config.TestClonesSpec{Clones: config.ClonesSetting{Auto: true}, NameTemplate: "mxpg_ca_{slug}_w{n}"},
		}},
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "postgres", false)
	want := int(testfw.DetectedCloneCount(wt))
	if want == 0 {
		want = int(testfw.NumCPUs())
	}
	if len(o.Clones) != want {
		t.Fatalf("clones:auto produced %d clones, want %d", len(o.Clones), want)
	}
	for _, c := range o.Clones {
		if !pgDBExists(t, c) {
			t.Errorf("auto clone %s missing", c)
		}
	}
}

// ─── password $ENV ref ───────────────────────────────────────────────

func TestPostgresPasswordEnvRef(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	wt := t.TempDir()
	write(t, wt, ".env", "PG_PW=pgpw\n")
	write(t, wt, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1);")
	write(t, wt, ".treeman.yaml", `connections:
  postgres:
    host: 127.0.0.1
    port: 15440
    user: postgres
    password: $PG_PW
env_sources: [.env]
databases:
  - engine: postgres
    name_template: mxpg_pw_{slug}
    dump: seed.sql
`)
	cfg, err := resolve.LoadResolved(wt)
	if err != nil {
		t.Fatalf("LoadResolved: %v", err)
	}
	// Resolution happened: the literal $PG_PW must be gone.
	if got := cfg.Connections.Postgres.Password; got != "pgpw" {
		t.Fatalf("password $ENV ref not resolved: got %q, want pgpw", got)
	}
	// And the resolved password actually authenticates (server requires it).
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, &cfg), "postgres", false)
	if !pgDBExists(t, o.SourceDB) {
		t.Errorf("password-ref: source DB %s not created", o.SourceDB)
	}
}

// ─── pool_max ────────────────────────────────────────────────────────

func TestPostgresPoolMaxCapsConcurrency(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	const poolMax = 2
	const goroutines = 8
	drv, err := dbpostgres.Connect(context.Background(), config.PostgresConn{
		Host: "127.0.0.1", Port: 15440, User: "postgres", Password: "pgpw", PoolMax: poolMax,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer drv.Close()

	var maxObserved int64
	stop := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		s, err := sql.Open("pgx", dsn("postgres"))
		if err != nil {
			return
		}
		defer s.Close()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var n int64
				if err := s.QueryRow(`SELECT count(*) FROM pg_stat_activity
					WHERE query LIKE '%poolmax_marker%' AND query NOT LIKE '%pg_stat_activity%' AND state = 'active'`).Scan(&n); err != nil {
					continue
				}
				for {
					cur := atomic.LoadInt64(&maxObserved)
					if n <= cur || atomic.CompareAndSwapInt64(&maxObserved, cur, n) {
						break
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var v int
			_ = drv.DB.QueryRowContext(ctx, "SELECT 1 /* poolmax_marker */ FROM pg_sleep(0.5)").Scan(&v)
		}()
	}
	wg.Wait()
	close(stop)
	samplerWG.Wait()

	observed := atomic.LoadInt64(&maxObserved)
	t.Logf("postgres max concurrent in-flight: %d (pool_max=%d)", observed, poolMax)
	if observed > poolMax {
		t.Errorf("PoolMax breach: observed %d > %d", observed, poolMax)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func dsn(db string) string {
	return "postgres://postgres:pgpw@" + pgAddr + "/" + db + "?sslmode=disable"
}

func pgDBExists(t *testing.T, name string) bool {
	t.Helper()
	db, _ := sql.Open("pgx", dsn("postgres"))
	defer db.Close()
	var n int
	_ = db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n)
	return n == 1
}

func composeProject(t *testing.T, container string) string {
	t.Helper()
	out, err := execOut("docker", "inspect", "--format",
		`{{index .Config.Labels "com.docker.compose.project"}}`, container)
	if err != nil {
		t.Fatalf("read compose project: %v", err)
	}
	p := trim(out)
	if p == "" {
		t.Fatal("empty compose project label")
	}
	return p
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func execOut(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func trim(s string) string { return strings.TrimSpace(s) }
