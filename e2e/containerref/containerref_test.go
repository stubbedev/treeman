//go:build e2e

// Package containerref_e2e drives treeman against MySQL containers
// referenced by NAME (not host:port), exercising containerip.ResolveAddr:
//
//   • TestPublishedPort       — container has -p 13366:3306; treeman
//     must dial 127.0.0.1:13366.
//   • TestBridgeNetworkIP     — container has no ports: block;
//     treeman must dial the bridge-network IP + internal 3306.
//
// Both omit `host:` from the YAML to force the resolution path.
package containerref_e2e

import (
	"context"
	"net"
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

func bothUp(t *testing.T) {
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))
	harness.WaitForReady(t, "mysql-published:13366", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13366", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
	// mysql-internal: confirm container is healthy via docker
	// inspect since no host port is published.
	harness.WaitForReady(t, "mysql-internal", 60*time.Second, func() error {
		out, err := exec.Command("docker", "inspect",
			"--format", "{{.State.Health.Status}}",
			"treeman-e2e-ctr-internal").CombinedOutput()
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "healthy" {
			return mkErr("not yet healthy: " + strings.TrimSpace(string(out)))
		}
		return nil
	})
}

func TestContainerRefPublishedPort(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bothUp(t)

	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2),(3);"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Note: NO host: — treeman must resolve container ref.
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{
				User:     "root",
				Password: "rootpw",
				ContainerRef: config.ContainerRef{
					Container: "treeman-e2e-ctr-published",
				},
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "tm_ctr_pub_{slug}",
				Dump:         &config.DumpSpec{Path: "seed.sql"},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "mysql", false)
	t.Logf("published path resolved → source=%s", o.SourceDB)
}

func TestContainerRefBridgeIP(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if runtime.GOOS != "linux" {
		// Docker for Mac / Windows hides the bridge network from the
		// host; only the published-port path is reachable from
		// outside the VM. On Linux the bridge IP is directly
		// routable, which is what makes this case interesting.
		t.Skip("bridge-network IP fallback only routable on Linux hosts")
	}
	bothUp(t)

	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE t (id INT); INSERT INTO t VALUES (10),(20);"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{
				User:     "root",
				Password: "rootpw",
				ContainerRef: config.ContainerRef{
					Container: "treeman-e2e-ctr-internal",
				},
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "tm_ctr_brg_{slug}",
				Dump:         &config.DumpSpec{Path: "seed.sql"},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "mysql", false)
	t.Logf("bridge-IP path resolved → source=%s", o.SourceDB)
}

func mkErr(s string) error { return &strErr{s} }

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }

var _ = context.Background
