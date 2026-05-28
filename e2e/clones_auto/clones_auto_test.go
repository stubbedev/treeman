//go:build e2e

// Package clones_auto_e2e exercises `test_clones.clones: auto` — the
// branch where treeman reads the project's test-runner config to decide
// how many paratest clone databases to pre-warm (instead of a fixed
// integer). The other suites only cover explicit counts.
//
// Two detection strategies are covered against a real MySQL:
//
//   - a CloneShared framework (phpunit) → exactly 1 clone.
//   - a ClonePerWorker framework (jest)  → NumCPUs clones.
//
// The expected count is computed from the same testfw detector prepare
// uses, so the assertion proves `clones: auto` actually wires through to
// detection and fans out that many real clone databases.
package clones_auto_e2e

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
)

const mysqlAddr = "127.0.0.1:13340"

func TestClonesAuto(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:"+mysqlAddr, 60*time.Second, func() error {
		db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})

	t.Run("shared_framework_one_clone", func(t *testing.T) {
		// composer.json with phpunit (and no paratest/pest) → CloneShared
		// → exactly one clone.
		runAutoCase(t, "tmca_shared", map[string]string{
			"composer.json": `{"require-dev":{"phpunit/phpunit":"^11"}}`,
		})
	})

	t.Run("per_worker_framework_numcpus", func(t *testing.T) {
		// package.json with jest → ClonePerWorker → NumCPUs clones.
		runAutoCase(t, "tmca_perworker", map[string]string{
			"package.json": `{"devDependencies":{"jest":"^29"}}`,
		})
	})
}

// runAutoCase lays down the given test-runner marker files + a seed dump
// in a fresh worktree, runs prepare with `clones: auto`, and asserts the
// number of clone databases created matches what the testfw detector
// reports for that worktree.
func runAutoCase(t *testing.T, prefix string, markers map[string]string) {
	t.Helper()
	wt := t.TempDir()
	for name, body := range markers {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2),(3);"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13340, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: prefix + "_{slug}",
			Dump:         &config.DumpSpec{Path: "seed.sql"},
			TestClones: &config.TestClonesSpec{
				Clones:       config.ClonesSetting{Auto: true},
				NameTemplate: prefix + "_{slug}_w{n}",
			},
		}},
	}

	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "mysql", false)

	want := int(testfw.DetectedCloneCount(wt))
	if want == 0 {
		want = int(testfw.NumCPUs()) // matches resolveCloneNames' fallback
	}
	if len(o.Clones) != want {
		t.Fatalf("clones: auto produced %d clones, want %d (detector count)", len(o.Clones), want)
	}
	if want < 1 {
		t.Fatalf("precondition: detector returned %d clones", want)
	}
	// Every reported clone must actually exist as a database.
	for _, c := range o.Clones {
		if !dbExists(t, c) {
			t.Errorf("clone database %s was reported but does not exist", c)
		}
	}
	t.Logf("clones: auto -> %d clones (%v)", len(o.Clones), o.Clones)
}

func dbExists(t *testing.T, name string) bool {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&n); err != nil {
		t.Fatalf("schema check %s: %v", name, err)
	}
	return n == 1
}
