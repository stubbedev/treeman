//go:build e2e

// Package containerref_all_e2e covers containerip.ResolveAddr against
// every engine in both modes:
//
//   - published-port (-p HOST:CONTAINER) — most common dev setup
//   - bridge-network IP (no ports: block) — Linux-routable fallback
//
// 10 containers boot once (one compose), 10 subtests run against
// them. The non-mysql engines run the same minimal happy path: drop
// stale namespace → load tiny seed → snapshot → assert. Each subtest
// uses a unique name template so they don't collide.
package containerref_all_e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

func TestContainerRefAllEngines(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	// Compose `--wait` already gates on healthcheck — but ES is
	// slow to settle, so re-poll the health status by inspect.
	waitContainer := func(name string, timeout time.Duration) {
		harness.WaitForReady(t, "inspect:"+name, timeout, func() error {
			out, err := exec.Command("docker", "inspect",
				"--format", "{{.State.Health.Status}}", name).CombinedOutput()
			if err != nil {
				return err
			}
			s := strings.TrimSpace(string(out))
			if s != "healthy" {
				return mkErr(s)
			}
			return nil
		})
	}
	for _, c := range []string{
		"treeman-e2e-ctra-mysql-pub", "treeman-e2e-ctra-mysql-int",
		"treeman-e2e-ctra-pg-pub", "treeman-e2e-ctra-pg-int",
		"treeman-e2e-ctra-mongo-pub", "treeman-e2e-ctra-mongo-int",
		"treeman-e2e-ctra-redis-pub", "treeman-e2e-ctra-redis-int",
		"treeman-e2e-ctra-es-pub", "treeman-e2e-ctra-es-int",
	} {
		waitContainer(c, 90*time.Second)
	}

	linuxOnly := runtime.GOOS != "linux"

	cases := []struct {
		name      string
		container string
		bridgeIP  bool
		cfgFn     func(wt string, ref config.ContainerRef) *config.Config
	}{
		{"mysql_published", "treeman-e2e-ctra-mysql-pub", false, mysqlConfig},
		{"mysql_bridge", "treeman-e2e-ctra-mysql-int", true, mysqlConfig},
		{"postgres_published", "treeman-e2e-ctra-pg-pub", false, postgresConfig},
		{"postgres_bridge", "treeman-e2e-ctra-pg-int", true, postgresConfig},
		{"mongo_published", "treeman-e2e-ctra-mongo-pub", false, mongoConfig},
		{"mongo_bridge", "treeman-e2e-ctra-mongo-int", true, mongoConfig},
		{"redis_published", "treeman-e2e-ctra-redis-pub", false, redisConfig},
		{"redis_bridge", "treeman-e2e-ctra-redis-int", true, redisConfig},
		{"elasticsearch_published", "treeman-e2e-ctra-es-pub", false, esConfig},
		{"elasticsearch_bridge", "treeman-e2e-ctra-es-int", true, esConfig},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.bridgeIP && linuxOnly {
				t.Skip("bridge-network IP fallback only routable on Linux hosts")
			}
			wt := t.TempDir()
			ref := config.ContainerRef{Container: c.container}
			cfg := c.cfgFn(wt, ref)
			env := harness.NewEnv(t, wt)
			outs := env.RunPrepare(t, cfg)
			if len(outs) == 0 {
				t.Fatalf("no outcomes; treeman did not run prepare")
			}
			o := outs[0]
			t.Logf("%s → engine=%s source=%s template=%s", c.name, o.Engine, o.SourceDB, o.TemplateName)
		})
	}
}

func mysqlConfig(wt string, ref config.ContainerRef) *config.Config {
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2);"), 0o644); err != nil {
		panic(err)
	}
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{User: "root", Password: "rootpw", ContainerRef: ref},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine: "mysql", NameTemplate: sanitize(ref.Container) + "_{slug}",
				Dump: &config.DumpSpec{Path: "seed.sql"},
			},
		},
	}
}

func postgresConfig(wt string, ref config.ContainerRef) *config.Config {
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2);"), 0o644); err != nil {
		panic(err)
	}
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{User: "postgres", Password: "pgpw", ContainerRef: ref},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine: "postgres", NameTemplate: sanitize(ref.Container) + "_{slug}",
				Dump: &config.DumpSpec{Path: "seed.sql"},
			},
		},
	}
}

func mongoConfig(_ string, ref config.ContainerRef) *config.Config {
	// For mongo we exercise the seed-less path: prepareMongo drops
	// the worktree DB and snapshots an empty template. That's enough
	// to verify the container-ref dial works.
	return &config.Config{
		Connections: config.ConnectionsConfig{
			// URI host is a sentinel — containerip rewrites at dial.
			Mongodb: &config.MongoConn{URI: "mongodb://placeholder:27017", ContainerRef: ref},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine: "mongodb", NameTemplate: sanitize(ref.Container) + "_{slug}",
			},
		},
	}
}

func redisConfig(_ string, ref config.ContainerRef) *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{URL: "redis://placeholder:6379", ContainerRef: ref},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "redis",
				NameTemplate: sanitize(ref.Container) + "_{slug}",
				KeyPrefix:    "ctra:{slug}:",
				// No seed — just confirms the dial+snapshot path works.
			},
		},
	}
}

func esConfig(_ string, ref config.ContainerRef) *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Elasticsearch: &config.EsConn{URL: "http://placeholder:9200", ContainerRef: ref},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "elasticsearch",
				NameTemplate: sanitize(ref.Container) + "_{slug}",
				KeyPrefix:    "ctra_{slug}_",
			},
		},
	}
}

// sanitize replaces dashes with underscores so the container name
// can be used in a SQL/Mongo database name (engines often forbid
// `-` in identifiers).
func sanitize(s string) string {
	return strings.ReplaceAll(s, "-", "_")
}

func mkErr(s string) error { return &strErr{s} }

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }

// Unused but imported defensively in case a sub-test wants to talk
// HTTP to ES directly.
var _ = json.Marshal
var _ = io.Discard
var _ = http.DefaultClient
var _ = context.Background
