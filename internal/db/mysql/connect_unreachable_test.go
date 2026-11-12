package mysql

import (
	"context"
	"errors"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/fakeserver"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Regression for #212: a server that accepts TCP and closes before the
// handshake (docker's proxy in front of a mysqld still starting) is an
// engine-unreachable failure, not an opaque "invalid connection".
func TestConnectDroppedHandshakeIsUnreachable(t *testing.T) {
	host, port := fakeserver.Start(t, fakeserver.Close)
	_, err := Connect(context.Background(), config.MysqlConn{Host: host, Port: port, User: "root"})
	ue, ok := errors.AsType[*reachability.UnreachableError](err)
	if !ok || ue.Reason != reachability.HandshakeDropped {
		t.Fatalf("Connect = %v, want a dropped-handshake *UnreachableError", err)
	}
}
