//go:build e2e

package fw_typeorm_e2e

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

const nodeContainer = "treeman-e2e-fw-typeorm-node"

func TestTypeORMEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15492", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15492", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := filepath.Join("/tmp", fmt.Sprintf("treeman-typeorm-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(wt) })
	copyTree(t, "fixtures", wt)

	t.Log("npm install (typeorm) inside node container...")
	cmd := exec.Command("docker", "exec", "-w", wt, nodeContainer,
		"npm", "install", "--no-audit", "--no-fund", "--loglevel=error")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("npm install: %v", err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: 15492,
				User: "postgres", Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "postgres",
				NameTemplate: "tm_to_{slug}",
				Migrate: &config.Step{
					Run: fmt.Sprintf(
						`docker exec -w %s -e DATABASE_URL=postgres://postgres:pgpw@postgres:5432/${DB_NAME} %s npx --no-install typeorm-ts-node-commonjs migration:run -d data-source.ts`,
						wt,
						nodeContainer,
					),
					Env: map[string]string{"DB_NAME": "{target_db}"},
				},
				Inputs: []config.Input{
					{Glob: "migrations/*.ts", Label: "migrations"},
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "postgres", false)
	t.Logf("source=%s template=%s", o.SourceDB, o.TemplateName)

	dsn := fmt.Sprintf("postgres://postgres:pgpw@127.0.0.1:15492/%s?sslmode=disable", o.SourceDB)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'widget'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n == 0 {
		t.Errorf("widget table not created by typeorm migration:run")
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

var _ = context.Background
