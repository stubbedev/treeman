package adapter

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
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

func endpoint(_ context.Context, h string, p uint16) (string, error) {
	return net.JoinHostPort(h, strconv.Itoa(int(p))), nil
}

func listenerPort(t *testing.T, l net.Listener) uint16 {
	t.Helper()
	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	p, _ := strconv.Atoi(portStr)
	return uint16(p) //nolint:gosec // listener port fits uint16
}

// TestConnectPlainHost covers the no-container path: a live listener
// probes clean and connect gets host/port untouched; a closed port
// surfaces a refusal instead of hanging.
func TestConnectPlainHost(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := listenerPort(t, l)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Connect(ctx, "mysql", containerip.Opts{}, "127.0.0.1", port, endpoint)
	if err != nil {
		t.Fatalf("live listener: %v", err)
	}
	if want := l.Addr().String(); got != want {
		t.Errorf("connect got %s without container opts, want %s", got, want)
	}

	_ = l.Close()
	start := time.Now()
	_, err = Connect(context.Background(), "mysql", containerip.Opts{}, "127.0.0.1", port, endpoint)
	if ue, ok := errors.AsType[*reachability.UnreachableError](err); !ok || ue.Reason != reachability.Refused {
		t.Errorf("closed port: want a refused *UnreachableError, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("closed port without startup wait took %v, want an immediate failure", elapsed)
	}
}

// TestConnectWaitsForRefusedEngine: under a startup-wait ctx a refused
// port is retried until the caller's deadline instead of failing on
// the first dial, and the final error still names the refusal.
func TestConnectWaitsForRefusedEngine(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listenerPort(t, l)
	_ = l.Close()

	ctx, cancel := context.WithTimeout(reachability.WithStartupWait(context.Background()), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Connect(ctx, "mysql", containerip.Opts{}, "127.0.0.1", port, endpoint)
	elapsed := time.Since(start)
	if ue, ok := errors.AsType[*reachability.UnreachableError](err); !ok || ue.Reason != reachability.Refused {
		t.Fatalf("want a refused *UnreachableError after the wait, got %v", err)
	}
	if elapsed < 500*time.Millisecond {
		t.Errorf("returned after %v, want it to keep retrying until the 600ms deadline", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("returned after %v, want it bounded by the 600ms deadline", elapsed)
	}
}

// Regression for #212: under a startup-wait ctx a dropped handshake
// (docker's userland proxy accepting before the engine listens) is
// waited out like a refusal, while a failure the engine reports ends
// the wait at once.
func TestConnectWaitsOutDroppedHandshakes(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := listenerPort(t, l)
	ctx, cancel := context.WithTimeout(reachability.WithStartupWait(context.Background()), 10*time.Second)
	defer cancel()

	attempts := 0
	got, err := Connect(ctx, "mysql", containerip.Opts{}, "127.0.0.1", port, func(ctx context.Context, _ string, _ uint16) (int, error) {
		attempts++
		if attempts < 3 {
			return 0, reachability.HandshakeError(ctx, "mysql", l.Addr().String(), io.EOF)
		}
		return attempts, nil
	})
	if err != nil || got != 3 {
		t.Fatalf("Connect = %d, %v; want the third attempt to succeed", got, err)
	}

	denied := errors.New("Error 1045 (28000): Access denied")
	attempts = 0
	_, err = Connect(ctx, "mysql", containerip.Opts{}, "127.0.0.1", port, func(ctx context.Context, _ string, _ uint16) (int, error) {
		attempts++
		return 0, reachability.HandshakeError(ctx, "mysql", l.Addr().String(), denied)
	})
	if !errors.Is(err, denied) || attempts != 1 {
		t.Errorf("Connect = %v after %d attempts, want the authentication failure at once", err, attempts)
	}
}

// TestConnectURLKeepsURIWithoutContainer: with no container reference
// connect gets the URI byte-identical, and the dialled address.
func TestConnectURLKeepsURIWithoutContainer(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	uri := "http://" + l.Addr().String() + "/"
	var gotURI, gotAddr string
	_, err = ConnectURL(
		context.Background(),
		"elasticsearch",
		containerip.Opts{},
		uri,
		func(_ context.Context, u, addr string) (struct{}, error) {
			gotURI, gotAddr = u, addr
			return struct{}{}, nil
		},
	)
	if err != nil {
		t.Fatalf("live listener: %v", err)
	}
	if gotURI != uri || gotAddr != l.Addr().String() {
		t.Errorf("connect got (%q, %q), want (%q, %q)", gotURI, gotAddr, uri, l.Addr().String())
	}
}

// stubConnector satisfies driver.Connector without ever connecting —
// ConfigurePool only touches pool accounting, not live connections.
type stubConnector struct{}

func (stubConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("stub")
}
func (stubConnector) Driver() driver.Driver { return nil }
