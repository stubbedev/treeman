//go:build e2e

// Package mysql_e2e tests the treeman MySQL prepare path against a
// real mysqld brought up via docker-compose. Two scenarios:
//
//  1. Cold build → cache hit: identical configs back-to-back. The
//     second prepare must hit the snapshot cache.
//  2. Cold rebuild on input edit: rewriting a migration file MUST
//     bust the fingerprint and force a fresh prepare.
package mysql_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

func TestMySQLEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)

	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	// Wait until mysqld is actually accepting connections (compose's
	// healthcheck handles this via --wait, but belt+braces).
	harness.WaitForReady(t, "mysql:13306", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13306", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	// Copy fixtures into the worktree so paths inside .treeman.yaml
	// (relative to the worktree) resolve.
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))

	cfg := buildConfig()
	env := harness.NewEnv(t, wt)

	// ── Pass 1: cold build. ──
	outs := env.RunPrepare(t, cfg)
	o1 := harness.AssertOutcome(t, outs, "mysql", false)
	t.Logf("pass1: template=%s clones=%v", o1.TemplateName, o1.Clones)

	assertTablesPresent(t, "127.0.0.1:13306", o1.SourceDB, []string{"products", "orders"})
	assertRowCount(t, "127.0.0.1:13306", o1.SourceDB, "products", 3)

	// ── Pass 2: cache hit. Same fingerprint → snapshot cache lookup
	//          should succeed without rebuilding.
	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "mysql", true)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("fingerprint changed between identical runs: %s vs %s", o1.Fingerprint, o2.Fingerprint)
	}

	// ── Pass 3: edit a migration → fingerprint must change.
	editMigration(t, wt)
	outs = env.RunPrepare(t, cfg)
	o3 := harness.AssertOutcome(t, outs, "mysql", false)
	if o3.Fingerprint == o1.Fingerprint {
		t.Errorf("fingerprint unchanged after migration edit (input hashing broken)")
	}
	assertTablesPresent(t, "127.0.0.1:13306", o3.SourceDB, []string{"products", "orders", "shipments"})
}

func buildConfig() *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{
				Host:     "127.0.0.1",
				Port:     13306,
				User:     "root",
				Password: "rootpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "treeman_e2e_{slug}",
				Dump:         &config.DumpSpec{Path: "fixtures/seed.sql"},
				Migrate: &config.Step{
					Run: "./fixtures/migrate.sh",
					Env: map[string]string{
						"DB_DATABASE":     "{target_db}",
						"MYSQL_CONTAINER": "treeman-e2e-mysql",
					},
				},
				Inputs: []config.Input{
					{Glob: "fixtures/migrations/*.sql", Label: "migrations", Hash: "filename"},
				},
			},
		},
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyTree(t, srcPath, dstPath)
			continue
		}
		body, err := os.ReadFile(srcPath)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := e.Info()
		mode := os.FileMode(0o644)
		if info != nil {
			mode = info.Mode().Perm()
		}
		if err := os.WriteFile(dstPath, body, mode); err != nil {
			t.Fatal(err)
		}
	}
}

func editMigration(t *testing.T, wt string) {
	t.Helper()
	// Add a new migration file — hash mode is "filename", so a new
	// filename in the dir flips the fingerprint regardless of
	// content. We add content too for realism.
	body := `CREATE TABLE shipments (
  id INT PRIMARY KEY AUTO_INCREMENT,
  order_id INT NOT NULL,
  shipped_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB;
`
	p := filepath.Join(wt, "fixtures/migrations/2024_02_01_000001_add_shipments.sql")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertTablesPresent(t *testing.T, addr, dbName string, want []string) {
	t.Helper()
	db := openMySQL(t, addr, dbName)
	defer db.Close()
	got := map[string]bool{}
	rows, err := db.QueryContext(context.Background(),
		"SELECT table_name FROM information_schema.tables WHERE table_schema = ?", dbName)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		got[name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("table %q missing from %s (have: %v)", w, dbName, keysOf(got))
		}
	}
}

func assertRowCount(t *testing.T, addr, dbName, table string, want int) {
	t.Helper()
	db := openMySQL(t, addr, dbName)
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		fmt.Sprintf("SELECT COUNT(*) FROM `%s`", table)).Scan(&n); err != nil {
		t.Fatalf("count(%s): %v", table, err)
	}
	if n != want {
		t.Errorf("%s.%s rows = %d, want %d", dbName, table, n, want)
	}
}

func openMySQL(t *testing.T, addr, dbName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root:rootpw@tcp(%s)/%s?parseTime=true", addr, dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
