//go:build e2e

// Package fw_django_e2e drives a minimal Django project through
// treeman against Postgres. python+pip aren't on the host, so the
// migrate CLI runs inside a python:3.12-slim sidecar that has
// django + psycopg2 pre-installed by compose.
package fw_django_e2e

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

const pyContainer = "treeman-e2e-fw-django-py"

func TestDjangoEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15462", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15462", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
	// Wait for the django container's pip install to finish.
	harness.WaitForReady(t, "django-pip", 120*time.Second, func() error {
		out, err := exec.Command("docker", "exec", pyContainer,
			"python", "-c", "import django, psycopg2").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	})

	wt := filepath.Join("/tmp", fmt.Sprintf("treeman-django-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(wt) })
	copyTree(t, "fixtures", wt)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: 15462,
				User: "postgres", Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "postgres",
				NameTemplate: "tm_dj_{slug}",
				Migrate: &config.Step{
					Run: fmt.Sprintf(
						`docker exec -w %s `+
							`-e DJANGO_DB_HOST=postgres -e DJANGO_DB_PORT=5432 `+
							`-e DJANGO_DB_USER=postgres -e DJANGO_DB_PASSWORD=pgpw `+
							`-e DJANGO_DB_NAME=${DB_NAME} `+
							`%s python manage.py migrate --noinput`,
						wt, pyContainer,
					),
					Env: map[string]string{"DB_NAME": "{target_db}"},
				},
				Inputs: []config.Input{
					{Glob: "core/migrations/*.py", Label: "migrations", Hash: "filename"},
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "postgres", false)
	t.Logf("source=%s template=%s", o.SourceDB, o.TemplateName)

	dsn := fmt.Sprintf("postgres://postgres:pgpw@127.0.0.1:15462/%s?sslmode=disable", o.SourceDB)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'core_widget'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n == 0 {
		t.Errorf("core_widget table not created by django migrate")
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
