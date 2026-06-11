//go:build e2e

package mysql_e2e

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

// waitMySQL blocks until the e2e mysqld accepts a TCP connection.
func waitMySQL(t *testing.T) {
	t.Helper()
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13306", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13306", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
}

func columnExists(t *testing.T, addr, dbName, table, column string) bool {
	t.Helper()
	db := openMySQL(t, addr, dbName)
	defer db.Close()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = ? AND column_name = ?`,
		dbName, table, column).Scan(&n)
	if err != nil {
		t.Fatalf("column lookup: %v", err)
	}
	return n > 0
}

// TestDumpOnlyRebuildOnEdit proves that editing an EXISTING migration
// (whose ledger row is NOT baked into the dump) rebuilds via the
// dump-only fast path: clone the post-dump intermediate template and
// re-run the full forward migrate, rather than reloading the dump or
// reusing the prior full template. The edited schema must be present.
func TestDumpOnlyRebuildOnEdit(t *testing.T) {
	waitMySQL(t)

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	env := harness.NewEnv(t, wt)

	// Pass 1 — cold build. Loads seed.sql (no baked ledger) then runs
	// migrate.sh against an empty ledger, applying the orders migration.
	// This also seeds the dump-only intermediate template.
	o1 := harness.AssertOutcome(t, env.RunPrepare(t, buildConfig()), "mysql", false)
	if o1.IncrementalBase != "" {
		t.Fatalf("pass 1 cold build must not be incremental; base=%s", o1.IncrementalBase)
	}

	// Edit the EXISTING orders migration in place — add a `note` column.
	// The content hash diverges mid-sequence, so the strict-prefix
	// (append) ancestor path can't match.
	editOrdersMigration(t, wt)

	o2 := harness.AssertOutcome(t, env.RunPrepare(t, buildConfig()), "mysql", false)
	if o2.Fingerprint == o1.Fingerprint {
		t.Fatalf("edit must flip the fingerprint; still %s", o1.Fingerprint[:12])
	}
	// Rollback is NOT configured here, and append-incremental can't match
	// an edited file, so an incremental base that is set AND differs from
	// the prior full template proves the dump-only path was taken.
	if o2.IncrementalBase == "" {
		t.Errorf("expected dump-only rebuild (IncrementalBase set), got cold build")
	}
	if o2.IncrementalBase == o1.Fingerprint {
		t.Errorf("dump-only must build off the dump-only template, not the prior full template %s", o1.Fingerprint[:12])
	}
	if !columnExists(t, "127.0.0.1:13306", o2.SourceDB, "orders", "note") {
		t.Errorf("edited migration not applied: orders.note column missing in %s", o2.SourceDB)
	}
}

func editOrdersMigration(t *testing.T, wt string) {
	t.Helper()
	body := `CREATE TABLE orders (
  id INT PRIMARY KEY AUTO_INCREMENT,
  product_id INT NOT NULL,
  qty INT NOT NULL,
  note VARCHAR(50),
  FOREIGN KEY (product_id) REFERENCES products(id)
) ENGINE=InnoDB;
`
	p := filepath.Join(wt, "fixtures/migrations/2024_01_01_000001_add_orders.sql")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackRebuildOnDumpBakedEdit proves the headline feature: when a
// migration's ledger row is BAKED INTO THE DUMP, a plain forward
// re-migrate (cold or dump-only) skips it — only the opt-in rollback
// path re-applies the edit. It clones the prior template, unwinds the
// changed tail via the rollback command (TREEMAN_ROLLBACK_STEPS=1), then
// re-runs migrate forward.
func TestRollbackRebuildOnDumpBakedEdit(t *testing.T) {
	waitMySQL(t)

	wt := t.TempDir()
	writeRollbackFixtures(t, wt)
	env := harness.NewEnv(t, wt)

	// Pass 1 — cold build. The dump bakes the ledger row for m1.sql, so
	// migrate.sh skips it; widget keeps the dump's OLD schema (no extra).
	o1 := harness.AssertOutcome(t, env.RunPrepare(t, buildRollbackConfig()), "mysql", false)
	if columnExists(t, "127.0.0.1:13306", o1.SourceDB, "widget", "extra") {
		t.Fatalf("pass 1 baseline wrong: widget.extra should NOT exist yet")
	}

	// Edit the already-applied (dump-baked) migration: add `extra` column.
	writeWidgetMigration(t, wt, true)

	o2 := harness.AssertOutcome(t, env.RunPrepare(t, buildRollbackConfig()), "mysql", false)
	if o2.Fingerprint == o1.Fingerprint {
		t.Fatalf("edit must flip the fingerprint")
	}
	// Rollback runs first (before dump-only) and clones the prior FULL
	// template, so its IncrementalBase is the prior template's
	// fingerprint — distinguishing it from the dump-only path.
	if o2.IncrementalBase != o1.Fingerprint {
		t.Errorf("expected rollback off prior template %s; got IncrementalBase=%q",
			o1.Fingerprint[:12], o2.IncrementalBase)
	}
	if !columnExists(t, "127.0.0.1:13306", o2.SourceDB, "widget", "extra") {
		t.Errorf("rollback path failed to re-apply the dump-baked edit: widget.extra missing in %s", o2.SourceDB)
	}
}

func buildRollbackConfig() *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{
				Host: "127.0.0.1", Port: 13306, User: "root", Password: "rootpw",
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "treeman_e2e_rb_{slug}",
			Dump:         config.DumpList{{Path: "fixtures-rb/seed-baked.sql"}},
			Migrate: &config.Step{
				Run: "./fixtures-rb/migrate.sh",
				Env: map[string]string{"DB_DATABASE": "{target_db}", "MYSQL_CONTAINER": "treeman-e2e-mysql"},
			},
			Rollback: &config.Step{
				Run: "./fixtures-rb/rollback.sh",
				Env: map[string]string{"DB_DATABASE": "{target_db}", "MYSQL_CONTAINER": "treeman-e2e-mysql"},
			},
			Inputs: []config.Input{{Glob: "fixtures-rb/migrations/*.sql", Label: "migrations"}},
		}},
	}
}

func writeRollbackFixtures(t *testing.T, wt string) {
	t.Helper()
	dir := filepath.Join(wt, "fixtures-rb")
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Reuse the shared ledger-aware migrate.sh (points at sibling
	// migrations/ via dirname $0).
	migrateSh, err := os.ReadFile("fixtures/migrate.sh")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "migrate.sh"), string(migrateSh), 0o755)

	// Dump that BAKES the ledger: products + the ledger table with m1.sql
	// already recorded + widget in its OLD shape. A forward migrate sees
	// m1.sql as applied and skips it.
	mustWrite(t, filepath.Join(dir, "seed-baked.sql"), `CREATE TABLE products (
  id INT PRIMARY KEY AUTO_INCREMENT,
  name VARCHAR(255) NOT NULL
) ENGINE=InnoDB;
CREATE TABLE _treeman_e2e_migrations (filename VARCHAR(255) PRIMARY KEY);
INSERT INTO _treeman_e2e_migrations (filename) VALUES ('m1.sql');
CREATE TABLE widget (
  id INT PRIMARY KEY AUTO_INCREMENT,
  v VARCHAR(10)
) ENGINE=InnoDB;
`, 0o644)

	// rollback.sh: unwind the dump-baked m1 so the re-migrate re-applies
	// it. Asserts the injected step count is exactly 1.
	mustWrite(t, filepath.Join(dir, "rollback.sh"), `#!/usr/bin/env sh
set -eu
: "${DB_DATABASE:?}"
: "${TREEMAN_ROLLBACK_STEPS:?}"
if [ "$TREEMAN_ROLLBACK_STEPS" != "1" ]; then
  echo "unexpected TREEMAN_ROLLBACK_STEPS=$TREEMAN_ROLLBACK_STEPS" >&2
  exit 1
fi
container="${MYSQL_CONTAINER:-treeman-e2e-mysql}"
docker exec -i "$container" mysql --user=root --password=rootpw "$DB_DATABASE" <<'SQL'
DROP TABLE IF EXISTS widget;
DELETE FROM _treeman_e2e_migrations WHERE filename = 'm1.sql';
SQL
`, 0o755)

	writeWidgetMigration(t, wt, false)
}

// writeWidgetMigration writes m1.sql. When edited=false it matches the
// dump's baked widget shape; when edited=true it adds an `extra` column,
// modelling an in-place edit of an already-applied migration.
func writeWidgetMigration(t *testing.T, wt string, edited bool) {
	t.Helper()
	body := `CREATE TABLE widget (
  id INT PRIMARY KEY AUTO_INCREMENT,
  v VARCHAR(10)
) ENGINE=InnoDB;
`
	if edited {
		body = `CREATE TABLE widget (
  id INT PRIMARY KEY AUTO_INCREMENT,
  v VARCHAR(50),
  extra INT
) ENGINE=InnoDB;
`
	}
	mustWrite(t, filepath.Join(wt, "fixtures-rb/migrations/m1.sql"), body, 0o644)
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}
