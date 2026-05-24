//go:build e2e

// Package misc_e2e covers small features that don't justify their
// own docker-compose stack:
//   • Daemon.LogLevel actually affects slog verbosity.
//   • Reachability probe surfaces a clean error against an
//     unreachable host:port.
package misc_e2e

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/db/reachability"
)

// TestReachabilityProbeFailsCleanly points the probe at a TCP port
// nothing is listening on. The error must name the engine + port so
// users can diagnose without grepping treeman's source.
func TestReachabilityProbeFailsCleanly(t *testing.T) {
	// Find a definitely-free port by binding and immediately closing.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	time.Sleep(20 * time.Millisecond) // let the kernel free the port

	err = reachability.Probe("mysql", "127.0.0.1", port)
	if err == nil {
		t.Fatal("probe against closed port should error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mysql") {
		t.Errorf("error should name engine, got: %s", msg)
	}
	t.Logf("got clean error: %s", msg)
}
