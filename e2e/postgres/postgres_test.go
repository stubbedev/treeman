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
				Dump:         &config.DumpSpec{Path: "fixtures/seed.sql"},
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
