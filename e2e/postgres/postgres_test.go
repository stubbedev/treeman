//go:build e2e

package postgres_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
)

func TestPostgresEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	cfg := buildConfig()
	env := harness.NewEnv(t, wt)

	outs := env.RunPrepare(t, cfg)
	o1 := harness.AssertOutcome(t, outs, "postgres", false)
	t.Logf("pass1: template=%s sourceDB=%s", o1.TemplateName, o1.SourceDB)

	assertTables(t, "127.0.0.1:15432", o1.SourceDB, []string{"products", "orders"})

	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "postgres", true)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("fingerprint drift: %s vs %s", o1.Fingerprint, o2.Fingerprint)
	}

	addMigration(t, wt)
	outs = env.RunPrepare(t, cfg)
	o3 := harness.AssertOutcome(t, outs, "postgres", false)
	if o3.Fingerprint == o1.Fingerprint {
		t.Errorf("fingerprint unchanged after edit")
	}
	assertTables(t, "127.0.0.1:15432", o3.SourceDB, []string{"products", "orders", "shipments"})

	// Fanout setup permutation: the 2 declared clones exist and carry the
	// seeded+migrated schema (restored from the template).
	if len(o3.Clones) != 2 {
		t.Fatalf("fanout: got %d clones, want 2 (%v)", len(o3.Clones), o3.Clones)
	}
	for _, c := range o3.Clones {
		if !pgDBExists(t, "127.0.0.1:15432", c) {
			t.Errorf("clone DB %s missing after fanout", c)
		}
		assertTables(t, "127.0.0.1:15432", c, []string{"products", "orders", "shipments"})
	}

	// ── teardown: `wt delete` drops the per-worktree DBs, keeps the cache ──
	// TeardownDatabases is the DB layer of `treeman wt delete`. It must DROP
	// the source database AND every clone while leaving the fingerprint-keyed
	// template intact, so the next prepare with the same inputs is a cache hit.
	if err := prepare.TeardownDatabases(env.Ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	if pgDBExists(t, "127.0.0.1:15432", o3.SourceDB) {
		t.Errorf("source DB %s still exists after teardown", o3.SourceDB)
	}
	for _, c := range o3.Clones {
		if pgDBExists(t, "127.0.0.1:15432", c) {
			t.Errorf("clone DB %s still exists after teardown", c)
		}
	}
	// The fingerprint-keyed template must SURVIVE teardown so the next
	// worktree with the same inputs still hits the cache.
	if !pgDBExists(t, "127.0.0.1:15432", o3.TemplateName) {
		t.Errorf("template DB %s was dropped by teardown (cache must survive)", o3.TemplateName)
	}
}

// TestMultipleDumpsLoadInOrder covers `dump:` as a SEQUENCE. base.sql
// creates a table; extras.sql inserts rows that reference it. They must
// be loaded in the declared order or the second statement would fail
// against a missing table — proving the loader walks the list serially
// rather than racing or reordering.
func TestMultipleDumpsLoadInOrder(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	fixtures := filepath.Join(wt, "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(fixtures, "base.sql"),
		"CREATE TABLE widgets (id INT PRIMARY KEY, name TEXT);\nINSERT INTO widgets VALUES (1, 'Alpha');\n")
	mustWrite(t, filepath.Join(fixtures, "extras.sql"),
		"INSERT INTO widgets VALUES (2, 'Beta');\nINSERT INTO widgets VALUES (3, 'Gamma');\n")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: 15432, User: "postgres", Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "postgres",
			NameTemplate: "tm_mdump_{slug}",
			Dump: config.DumpList{
				{Path: "fixtures/base.sql"},
				{Path: "fixtures/extras.sql"},
			},
		}},
	}
	env := harness.NewEnv(t, wt)
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "postgres", false)
	t.Logf("multi-dump cold: source=%s fp=%s", o.SourceDB, o.Fingerprint[:12])
	assertTables(t, "127.0.0.1:15432", o.SourceDB, []string{"widgets"})
	assertWidgetCount(t, "127.0.0.1:15432", o.SourceDB, 3)

	// Reordering the same files MUST flip the fingerprint — the
	// combined hash is order-sensitive by design (different order
	// would mean different DB content). Use InspectFingerprint with a
	// blank engineVersion on both sides so the only varying input is
	// the dump order.
	rep1 := prepare.InspectFingerprint(env.Ctx, env.Store, cfg.Databases[0], wt, o.SourceDB, "")
	swappedCfg := cfg.Databases[0]
	swappedCfg.Dump = config.DumpList{
		{Path: "fixtures/extras.sql"},
		{Path: "fixtures/base.sql"},
	}
	rep2 := prepare.InspectFingerprint(env.Ctx, env.Store, swappedCfg, wt, o.SourceDB, "")
	if rep1.Fingerprint == rep2.Fingerprint {
		t.Errorf("reordering dumps must change the fingerprint (rep1=%s rep2=%s)",
			rep1.Fingerprint[:12], rep2.Fingerprint[:12])
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertWidgetCount(t *testing.T, addr, dbName string, want int) {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/%s?sslmode=disable", addr, dbName)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM widgets").Scan(&n); err != nil {
		t.Fatalf("count widgets in %s: %v", dbName, err)
	}
	if n != want {
		t.Errorf("widgets count in %s = %d, want %d", dbName, n, want)
	}
}

// pgDBExists reports whether a database named `name` lives in the
// cluster — checked from the `postgres` maintenance DB so it works even
// after `name` has been dropped.
func pgDBExists(t *testing.T, addr, name string) bool {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/postgres?sslmode=disable", addr)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg maintenance db: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n); err != nil {
		t.Fatalf("pg exists %s: %v", name, err)
	}
	return n == 1
}

func buildConfig() *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host:     "127.0.0.1",
				Port:     15432,
				User:     "postgres",
				Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "postgres",
				NameTemplate: "treeman_e2e_{slug}",
				Dump:         config.DumpList{{Path: "fixtures/seed.sql"}},
				Migrate: &config.Step{
					Run: "./fixtures/migrate.sh",
					Env: map[string]string{
						"DB_DATABASE":  "{target_db}",
						"PG_CONTAINER": "treeman-e2e-postgres",
					},
				},
				Inputs: []config.Input{
					{Glob: "fixtures/migrations/*.sql", Label: "migrations", Hash: "filename"},
				},
				// Fanout: pre-warm 2 paratest clones so teardown's
				// per-worktree cleanup (source + clones) is exercised.
				TestClones: &config.TestClonesSpec{
					Clones:       config.ClonesSetting{Fixed: 2},
					NameTemplate: "treeman_e2e_{slug}_w{n}",
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
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyTree(t, s, d)
			continue
		}
		body, _ := os.ReadFile(s)
		info, _ := e.Info()
		mode := os.FileMode(0o644)
		if info != nil {
			mode = info.Mode().Perm()
		}
		_ = os.WriteFile(d, body, mode)
	}
}

func addMigration(t *testing.T, wt string) {
	body := `CREATE TABLE shipments (id SERIAL PRIMARY KEY, order_id INT NOT NULL REFERENCES orders(id));` + "\n"
	if err := os.WriteFile(filepath.Join(wt, "fixtures/migrations/002_shipments.sql"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertTables(t *testing.T, addr, dbName string, want []string) {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/%s?sslmode=disable", addr, dbName)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	got := map[string]bool{}
	rows, err := db.QueryContext(context.Background(),
		"SELECT tablename FROM pg_tables WHERE schemaname = 'public'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("table %q missing from %s (have: %v)", w, dbName, keys(got))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
