//go:build e2e

package fw_sequelize_e2e

import (
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

const nodeContainer = "treeman-e2e-fw-seq-node"

func TestSequelizeEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15512", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15512", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
	wt := filepath.Join("/tmp", fmt.Sprintf("treeman-sequelize-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(wt) })
	copyTree(t, "fixtures", wt)

	t.Log("npm install sequelize...")
	if err := exec.Command("docker", "exec", "-w", wt, nodeContainer,
		"npm", "install", "--no-audit", "--no-fund", "--loglevel=error").Run(); err != nil {
		t.Fatalf("npm install: %v", err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: 15512,
				User: "postgres", Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "postgres",
				NameTemplate: "tm_sq_{slug}",
				Migrate: &config.Step{
					Run: fmt.Sprintf(
						`docker exec -w %s -e DATABASE_URL=postgres://postgres:pgpw@postgres:5432/${DB_NAME} %s npx --no-install sequelize-cli db:migrate`,
						wt, nodeContainer,
					),
					Env: map[string]string{"DB_NAME": "{target_db}"},
				},
				Inputs: []config.Input{
					{Glob: "migrations/*.js", Label: "migrations", Hash: "filename"},
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "postgres", false)
	t.Logf("source=%s", o.SourceDB)
	dsn := fmt.Sprintf("postgres://postgres:pgpw@127.0.0.1:15512/%s?sslmode=disable", o.SourceDB)
	db, _ := sql.Open("pgx", dsn)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name='widgets'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n == 0 {
		t.Errorf("widgets table not created")
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
		_ = os.WriteFile(d, body, 0o644)
	}
}
