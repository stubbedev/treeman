//go:build e2e

// Package fw_laravel_e2e drives a real composer-based PHP project
// through treeman's prepare flow. The fixture is a minimal Laravel-
// shaped layout (composer.json + artisan + database/migrations/) so
// `composer install` is fast (~15s) and the test stays under 1min.
//
// Verifies the Laravel preset's migrate.run + DB_DATABASE env-var
// redirection both work end-to-end against real MySQL.
package fw_laravel_e2e

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

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

func TestLaravelEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("php"); err != nil {
		t.Skip("php not installed")
	}
	if _, err := exec.LookPath("composer"); err != nil {
		t.Skip("composer not installed")
	}
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13396", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13396", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", wt)

	// composer install (one-time).
	t.Log("composer install...")
	cmd := exec.Command("composer", "install", "--no-interaction", "--prefer-dist", "--quiet")
	cmd.Dir = wt
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("composer install: %v", err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{
				Host: "127.0.0.1", Port: 13396,
				User: "root", Password: "rootpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "tm_lv_{slug}",
				// The Laravel preset's canonical migrate command:
				Migrate: &config.Step{
					Run: "php artisan migrate",
					Env: map[string]string{
						"DB_HOST":     "127.0.0.1",
						"DB_PORT":     "13396",
						"DB_USERNAME": "root",
						"DB_PASSWORD": "rootpw",
						"DB_DATABASE": "{target_db}",
					},
				},
				Inputs: []config.Input{
					{Glob: "database/migrations/*.php", Label: "migrations"},
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "mysql", false)
	t.Logf("source=%s template=%s", o.SourceDB, o.TemplateName)

	dsn := fmt.Sprintf("root:rootpw@tcp(127.0.0.1:13396)/%s", o.SourceDB)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow("SHOW TABLES LIKE 'widgets'").Scan(&name); err == sql.ErrNoRows {
		t.Fatal("widgets table not created by artisan migrate")
	} else if err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "widgets" {
		t.Errorf("table name = %q, want widgets", name)
	}

	// Cache hit on rerun.
	outs2 := env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs2, "mysql", true)
	if o2.Fingerprint != o.Fingerprint {
		t.Errorf("fingerprint drift on Laravel rerun")
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
