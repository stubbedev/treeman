package adapter

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// TestConfigurePoolAppliesLimits checks the pool knobs land on the
// handle: poolMax caps open conns with idle at half, and the zero
// value leaves MaxOpenConns unlimited but still sets lifetimes.
func TestConfigurePoolAppliesLimits(t *testing.T) {
	db := sql.OpenDB(stubConnector{})
	defer func() { _ = db.Close() }()
	ConfigurePool(db, 8)
	if got := db.Stats().MaxOpenConnections; got != 8 {
		t.Errorf("MaxOpenConnections = %d, want 8", got)
	}

	db2 := sql.OpenDB(stubConnector{})
	defer func() { _ = db2.Close() }()
	ConfigurePool(db2, 0)
	if got := db2.Stats().MaxOpenConnections; got != 0 {
		t.Errorf("poolMax=0 must leave MaxOpenConnections unlimited, got %d", got)
	}
}

// TestResolveAndProbePlainHost covers the no-container path: a live
// listener probes clean and host/port stay untouched; a closed port
// surfaces an error instead of hanging.
func TestResolveAndProbePlainHost(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	p, _ := strconv.Atoi(portStr)

	host := "127.0.0.1"
	port := uint16(p) //nolint:gosec // listener port fits uint16

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ResolveAndProbe(ctx, "mysql", containerip.Opts{}, &host, &port); err != nil {
		t.Fatalf("live listener: %v", err)
	}
	if host != "127.0.0.1" || int(port) != p {
		t.Errorf("host/port rewritten without container opts: %s:%d", host, port)
	}

	_ = l.Close()
	start := time.Now()
	if err := ResolveAndProbe(context.Background(), "mysql", containerip.Opts{}, &host, &port); !reachability.IsRefused(err) {
		t.Errorf("closed port: want refused probe error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("closed port without startup wait took %v, want an immediate failure", elapsed)
	}
}

// TestResolveAndProbeWaitsForRefusedEngine: under a startup-wait ctx a
// refused port is retried until the caller's deadline instead of
// failing on the first dial, and the final error still names the
// refusal.
func TestResolveAndProbeWaitsForRefusedEngine(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	p, _ := strconv.Atoi(portStr)
	host, port := "127.0.0.1", uint16(p) //nolint:gosec // listener port fits uint16

	ctx, cancel := context.WithTimeout(reachability.WithStartupWait(context.Background()), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = ResolveAndProbe(ctx, "mysql", containerip.Opts{}, &host, &port)
	elapsed := time.Since(start)
	if !reachability.IsRefused(err) {
		t.Fatalf("want refused error after the wait, got %v", err)
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("returned after %v, want it to keep retrying until the 600ms deadline", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("returned after %v, want it bounded by the 600ms deadline", elapsed)
	}
}

// TestResolveAndProbeURLKeepsURIWithoutContainer: with no container
// reference the URI comes back byte-identical.
func TestResolveAndProbeURLKeepsURIWithoutContainer(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	uri := "http://" + l.Addr().String() + "/"
	got, err := ResolveAndProbeURL(context.Background(), "elasticsearch", containerip.Opts{}, uri)
	if err != nil {
		t.Fatalf("live listener: %v", err)
	}
	if got != uri {
		t.Errorf("uri = %q, want %q unchanged", got, uri)
	}
}

// stubConnector satisfies driver.Connector without ever connecting —
// ConfigurePool only touches pool accounting, not live connections.
type stubConnector struct{}

func (stubConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("stub")
}
func (stubConnector) Driver() driver.Driver { return nil }
