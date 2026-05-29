//go:build e2e

package fw_rails_e2e

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

const rubyContainer = "treeman-e2e-fw-rails-ruby"

func TestRailsEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15482", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15482", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
	// Ruby gem install needs a generous window (libpq-dev compile +
	// gem install activerecord pg).
	harness.WaitForReady(t, "ruby-gems", 240*time.Second, func() error {
		out, err := exec.Command("docker", "exec", rubyContainer,
			"ruby", "-r", "active_record", "-r", "pg", "-e", "").CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	})

	wt := filepath.Join("/tmp", fmt.Sprintf("treeman-rails-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(wt) })
	copyTree(t, "fixtures", wt)
	// Make bin/rails executable inside the container's view.
	_ = os.Chmod(filepath.Join(wt, "bin/rails"), 0o755)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: 15482,
				User: "postgres", Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "postgres",
				NameTemplate: "tm_rl_{slug}",
				Migrate: &config.Step{
					Run: fmt.Sprintf(
						`docker exec -w %s -e DATABASE_URL=postgres://postgres:pgpw@postgres:5432/${DB_NAME} %s bin/rails db:migrate`,
						wt, rubyContainer,
					),
					Env: map[string]string{"DB_NAME": "{target_db}"},
				},
				Inputs: []config.Input{
					{Glob: "db/migrate/*.rb", Label: "migrations"},
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "postgres", false)
	t.Logf("source=%s template=%s", o.SourceDB, o.TemplateName)

	dsn := fmt.Sprintf("postgres://postgres:pgpw@127.0.0.1:15482/%s?sslmode=disable", o.SourceDB)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'widgets'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n == 0 {
		t.Errorf("widgets table not created by Rails migrate")
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

var _ = context.Background
