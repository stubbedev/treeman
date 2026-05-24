//go:build e2e

// Package ctrrestart_e2e exercises treeman's containerip cache
// eviction on container restart. Flow:
//
//   1. Resolve MySQL by container name → cached IP_old
//   2. Restart the container → docker assigns a new bridge IP
//   3. Resolve again → driver Connect() trips the reachability
//      probe (IP_old is unreachable), calls RefreshOpts, retries
//      via the fresh address, and reconnects successfully.
package ctrrestart_e2e

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
)

const containerName = "treeman-e2e-ctrrestart-mysql"

func TestContainerRestartReresolves(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if runtime.GOOS != "linux" {
		t.Skip("bridge-network IP path only routable on Linux hosts")
	}
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	// Wait for healthy.
	harness.WaitForReady(t, "container-healthy", 90*time.Second, func() error {
		out, err := exec.Command("docker", "inspect",
			"--format", "{{.State.Health.Status}}", containerName).CombinedOutput()
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "healthy" {
			return mkErr("not healthy yet")
		}
		return nil
	})

	cfg := config.MysqlConn{
		User:     "root",
		Password: "rootpw",
		ContainerRef: config.ContainerRef{
			Container: containerName,
		},
	}

	// First connect — caches the current bridge IP.
	drv1, err := dbmysql.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	addr1 := pingContainerIP(t)
	t.Logf("initial IP: %s", addr1)
	drv1.Close()

	// Take it down and recreate to force a fresh bridge IP. Docker
	// typically gives sequential IPs on creation, so the recreated
	// container almost always gets a different one.
	composeDir := harness.MustAbs(".")
	cmd := exec.Command("docker", "compose", "down", "-v")
	cmd.Dir = composeDir
	_ = cmd.Run()
	cmd = exec.Command("docker", "compose", "up", "-d", "--wait")
	cmd.Dir = composeDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	harness.WaitForReady(t, "container-healthy-after-recreate", 90*time.Second, func() error {
		out, err := exec.Command("docker", "inspect",
			"--format", "{{.State.Health.Status}}", containerName).CombinedOutput()
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "healthy" {
			return mkErr("not healthy yet")
		}
		return nil
	})

	addr2 := pingContainerIP(t)
	t.Logf("after-recreate IP: %s (changed from %s? %v)", addr2, addr1, addr1 != addr2)
	// Whether IP changed or not, treeman should reconnect cleanly.
	// If the cache held a stale IP, Connect's probe would fail and
	// the eviction-retry path must auto-recover.
	drv2, err := dbmysql.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("post-restart Connect: %v", err)
	}
	defer drv2.Close()

	// Sanity: actually query MySQL through the new connection.
	var v int
	if err := drv2.DB.QueryRow("SELECT 1").Scan(&v); err != nil {
		t.Fatalf("post-restart SELECT 1: %v", err)
	}
	if v != 1 {
		t.Errorf("SELECT 1 = %d", v)
	}
}

// pingContainerIP reads the live bridge-network IP via docker
// inspect. Helps the test log show whether the IP actually changed
// across restart (it usually does on Linux + bridge net).
func pingContainerIP(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect",
		"--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		containerName).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect IP: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func mkErr(s string) error { return &strErr{s} }

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }
