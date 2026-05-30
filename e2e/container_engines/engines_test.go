//go:build e2e

// Package containerengines_e2e proves treeman's `container_engine`
// config option drives the engine-agnostic CLI paths end-to-end
// against non-docker daemons. The interesting paths are:
//
//   - containerip.ResolveAddr → `<engine> inspect` (host/port discovery)
//   - dumpload fast path     → `<engine> exec -i <cid> <tool>` (psql, etc.)
//
// Both shell out to the configured engine binary by name, so the
// surface that could silently diverge between docker and an
// alternative is (a) the JSON shape of `inspect` and (b) the
// semantics of `exec -i`. Each sub-test below exercises one of those
// paths for each engine.
//
// Tests skip cleanly when an engine isn't installed or its daemon
// isn't reachable, so default `go test -tags=e2e ./e2e/...` on a
// docker-only box stays green. Run on a host that has the engine
// installed to actually exercise the parity claim.
package containerengines_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

// Fully-qualified registry path because rootless podman ships with
// no default registry — an unqualified `postgres:16-alpine` would
// prompt interactively or fail outright. nerdctl and finch accept
// the qualified form too.
const postgresImage = "docker.io/library/postgres:16-alpine"

// engineCase pins each engine to a unique host port + container name
// so the matrix can survive being run in environments where two
// engines are installed side by side (e.g. nerdctl + finch on the
// same Lima VM, or all three on a Linux dev box).
type engineCase struct {
	name     string
	hostPort uint16
}

func TestEngineParity(t *testing.T) {
	for _, ec := range []engineCase{
		{"podman", 15533},
		{"nerdctl", 15534},
		{"finch", 15535},
	} {
		t.Run(ec.name, func(t *testing.T) {
			harness.SkipIfNoEngine(t, ec.name)
			container := fmt.Sprintf("treeman-e2e-%s-pg", ec.name)
			startEnginePostgres(t, ec.name, container, ec.hostPort)

			t.Run("FastPathDumpLoad", func(t *testing.T) {
				runFastPathDumpLoad(t, ec.name, container, ec.hostPort)
			})
			t.Run("InspectResolvesPublishedPort", func(t *testing.T) {
				runInspectResolvesPublishedPort(t, ec.name, container, ec.hostPort)
			})
		})
	}
}

// startEnginePostgres brings up a Postgres container under `engine`
// and registers cleanup. Idempotent across re-runs — any stale
// container from a previous failed run is removed first.
func startEnginePostgres(t *testing.T, engine, container string, hostPort uint16) {
	t.Helper()
	_ = exec.Command(engine, "rm", "-f", container).Run()
	t.Cleanup(func() {
		_ = exec.Command(engine, "rm", "-f", container).Run()
	})
	out, err := exec.Command(engine, "run", "-d",
		"--name", container,
		"-p", fmt.Sprintf("%d:5432", hostPort),
		"-e", "POSTGRES_PASSWORD=pgpw",
		postgresImage,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("%s run: %v\n%s", engine, err, out)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", hostPort)
	harness.WaitForReady(t, engine+"-pg:"+addr, 120*time.Second, func() error {
		c, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		// Port-open precedes pg_isready by several seconds — gate on
		// the actual round-trip the way the mongo e2e does, to avoid
		// the cold-start race that bit us there.
		out, err := exec.Command(engine, "exec", container,
			"pg_isready", "-U", "postgres").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s pg_isready: %w (output: %s)", engine, err, out)
		}
		if !strings.Contains(string(out), "accepting connections") {
			return fmt.Errorf("%s pg_isready: %s", engine, out)
		}
		return nil
	})
}

// runFastPathDumpLoad drives the `<engine> exec -i <cid> psql` fast
// path. ContainerRef.Container + ContainerEngine=<engine> are what
// trigger it (see dumpload/native.go:tryDockerExecPostgres) —
// without them the loader falls through to the wire path and this
// test would prove nothing about the engine binary. Asserting the
// rows the dump CREATEd are present afterward proves the bytes
// really traversed `<engine> exec -i`'s stdin.
func runFastPathDumpLoad(t *testing.T, engine, container string, hostPort uint16) {
	t.Helper()
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE widgets (id INT PRIMARY KEY);\n"+
			"INSERT INTO widgets VALUES (1),(2),(3);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: hostPort,
				User: "postgres", Password: "pgpw",
				ContainerRef: config.ContainerRef{
					Container:       container,
					ContainerEngine: engine,
				},
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "postgres",
			NameTemplate: "tm_" + engine + "_{slug}",
			Dump:         config.DumpList{{Path: "seed.sql"}},
		}},
	}
	env := harness.NewEnv(t, wt)
	o1 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "postgres", false)
	t.Logf("%s cold: source=%s fp=%s", engine, o1.SourceDB, o1.Fingerprint[:12])

	addr := fmt.Sprintf("127.0.0.1:%d", hostPort)
	assertWidgetCount(t, addr, o1.SourceDB, 3)

	// Second pass: identical inputs → cache hit. Proves the
	// engine-agnostic fingerprint/template paths don't depend on
	// docker-specific state.
	o2 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "postgres", true)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("%s: fingerprint drift across passes: %s vs %s",
			engine, o1.Fingerprint[:12], o2.Fingerprint[:12])
	}
}

// runInspectResolvesPublishedPort drives containerip.ResolveAddr
// against the engine: the config OMITS Host/Port so the resolver
// MUST shell out to `<engine> inspect` and discover the published
// 5432→host port mapping. If the engine's inspect JSON shape
// diverged from docker's (NetworkSettings.Ports keys, HostIp/HostPort
// field names), the resolver would return zero and the prepare
// connect would fail.
func runInspectResolvesPublishedPort(t *testing.T, engine, container string, hostPort uint16) {
	t.Helper()
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE gadgets (id INT);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				// No Host / Port — forces containerip to resolve them
				// from `<engine> inspect`. Default in-container port
				// 5432 is what the resolver maps from.
				User: "postgres", Password: "pgpw",
				ContainerRef: config.ContainerRef{
					Container:       container,
					ContainerEngine: engine,
				},
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "postgres",
			NameTemplate: "tm_" + engine + "_inspect_{slug}",
			Dump:         config.DumpList{{Path: "seed.sql"}},
		}},
	}
	env := harness.NewEnv(t, wt)
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "postgres", false)

	// AssertOutcome already fatalled if inspect returned a wrong
	// address (connect would have failed). Belt-and-braces: reach
	// the DB via the same published port the resolver should have
	// found, from the test's POV.
	addr := fmt.Sprintf("127.0.0.1:%d", hostPort)
	if !pgDBExists(t, addr, o.SourceDB) {
		t.Errorf("%s: source DB %s not visible at %s — inspect resolution likely returned wrong addr",
			engine, o.SourceDB, addr)
	}
}

func assertWidgetCount(t *testing.T, addr, dbName string, want int) {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/%s?sslmode=disable", addr, dbName)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM widgets").Scan(&n); err != nil {
		t.Fatalf("count widgets in %s: %v", dbName, err)
	}
	if n != want {
		t.Errorf("widgets count in %s = %d, want %d", dbName, n, want)
	}
}

func pgDBExists(t *testing.T, addr, name string) bool {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/postgres?sslmode=disable", addr)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n); err != nil {
		t.Fatalf("pg exists %s: %v", name, err)
	}
	return n == 1
}
