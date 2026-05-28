//go:build e2e

// Package fw_golang_migrate_e2e drives treeman against the real
// golang-migrate CLI. The binary is installed via `go install` into
// a per-test tempdir so we don't pollute the host's PATH or
// $GOPATH/bin.
//
// Flow:
//  1. boot postgres
//  2. install migrate binary into tempdir
//  3. write treeman config with migrate.run = "<tempdir>/migrate up"
//  4. run prepare → expect migrate to apply *.up.sql
//  5. verify the created table exists
package fw_golang_migrate_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

func TestGolangMigrateEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed on PATH")
	}
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15442", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15442", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	binDir := t.TempDir()
	installMigrate(t, binDir)
	migratePath := filepath.Join(binDir, "migrate")

	wt := t.TempDir()
	// Migrations: *.up.sql + *.down.sql pairs, the golang-migrate
	// convention.
	migDir := filepath.Join(wt, "db", "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "001_create_widgets.up.sql"),
		[]byte(`CREATE TABLE widgets (id SERIAL PRIMARY KEY, name VARCHAR(64));
INSERT INTO widgets (name) VALUES ('alpha'), ('beta'), ('gamma');`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "001_create_widgets.down.sql"),
		[]byte(`DROP TABLE widgets;`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: 15442,
				User: "postgres", Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "postgres",
				NameTemplate: "tm_gm_{slug}",
				Migrate: &config.Step{
					Run: migratePath + ` -path db/migrations -database "postgres://postgres:pgpw@127.0.0.1:15442/${DB_NAME}?sslmode=disable" up`,
					Env: map[string]string{
						"DB_NAME": "{target_db}",
					},
				},
				Inputs: []config.Input{
					{Glob: "db/migrations/*.up.sql", Label: "migrations", Hash: "filename"},
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "postgres", false)
	t.Logf("source=%s template=%s", o.SourceDB, o.TemplateName)

	dsn := fmt.Sprintf("postgres://postgres:pgpw@127.0.0.1:15442/%s?sslmode=disable", o.SourceDB)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM widgets").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("widgets rows = %d, want 3", n)
	}
}

func installMigrate(t *testing.T, binDir string) {
	t.Helper()
	// `go install` honours GOBIN if set.
	cmd := exec.Command("go", "install",
		"-tags", "postgres",
		"github.com/golang-migrate/migrate/v4/cmd/migrate@v4.17.0")
	cmd.Env = append(os.Environ(), "GOBIN="+binDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go install migrate: %v", err)
	}
}

var _ = context.Background
