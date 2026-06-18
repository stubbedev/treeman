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
	deadCtx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := ResolveAndProbe(deadCtx, "mysql", containerip.Opts{}, &host, &port); err == nil {
		t.Error("closed port: want probe error")
	}
}

// stubConnector satisfies driver.Connector without ever connecting —
// ConfigurePool only touches pool accounting, not live connections.
type stubConnector struct{}

func (stubConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("stub")
}
func (stubConnector) Driver() driver.Driver { return nil }
