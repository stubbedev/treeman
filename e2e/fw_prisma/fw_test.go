//go:build e2e

// Package fw_prisma_e2e drives a Node + Prisma project through
// treeman against Postgres. Prisma's precompiled engine binaries
// are not portable across libc variants (notably broken on NixOS),
// so the npm/npx invocations run inside a node:20-slim sidecar
// container — the worktree dir is bind-mounted at /tmp so docker
// exec --workdir works against the test's TempDir.
package fw_prisma_e2e

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

const nodeContainer = "treeman-e2e-fw-prisma-node"

func TestPrismaEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15452", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15452", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	// Use a deterministic wt path under /tmp so the node container
	// (mounting /tmp) sees the same directory tree.
	wt := filepath.Join("/tmp", fmt.Sprintf("treeman-prisma-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(wt) })
	copyTree(t, "fixtures", wt)

	// `npm install` inside the node sidecar.
	t.Log("npm install (prisma) inside node container...")
	cmd := exec.Command("docker", "exec", "-w", wt, nodeContainer,
		"npm", "install", "--no-audit", "--no-fund", "--loglevel=error")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("npm install: %v", err)
	}

	// treeman's migrate.run shells out to docker exec into the
	// node container — that's how a real laravel-in-docker /
	// rails-in-docker workflow looks too.
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: 15452,
				User: "postgres", Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "postgres",
				NameTemplate: "tm_pr_{slug}",
				Migrate: &config.Step{
					// node container is on the same compose network
					// as postgres → reachable as `postgres:5432`
					// (the internal port, not the published one).
					Run: fmt.Sprintf(
						`docker exec -w %s -e DATABASE_URL="postgres://postgres:pgpw@postgres:5432/${DB_NAME}?sslmode=disable" %s npx --no-install prisma db push --skip-generate --accept-data-loss`,
						wt, nodeContainer,
					),
					Env: map[string]string{
						"DB_NAME": "{target_db}",
					},
				},
				Inputs: []config.Input{
					{Glob: "prisma/schema.prisma", Label: "schema"},
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "postgres", false)
	t.Logf("source=%s template=%s", o.SourceDB, o.TemplateName)

	dsn := fmt.Sprintf("postgres://postgres:pgpw@127.0.0.1:15452/%s?sslmode=disable", o.SourceDB)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'Widget'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n == 0 {
		t.Errorf("Widget table not created by prisma db push")
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
