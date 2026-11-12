package postgres

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/fakeserver"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// answerStartupWith reads the client's startup message and answers it
// with a FATAL ErrorResponse carrying sqlstate.
func answerStartupWith(sqlstate, message string) func(net.Conn) {
	return func(c net.Conn) {
		var n uint32
		if err := binary.Read(c, binary.BigEndian, &n); err != nil || n < 4 {
			return
		}
		if _, err := io.CopyN(io.Discard, c, int64(n-4)); err != nil {
			return
		}
		var fields []byte
		for _, f := range []struct {
			code byte
			val  string
		}{{'S', "FATAL"}, {'V', "FATAL"}, {'C', sqlstate}, {'M', message}} {
			fields = append(fields, f.code)
			fields = append(fields, f.val...)
			fields = append(fields, 0)
		}
		fields = append(fields, 0)
		msg := []byte{'E'}
		msg = binary.BigEndian.AppendUint32(msg, uint32(len(fields)+4)) //nolint:gosec // a handful of short fields
		_, _ = c.Write(append(msg, fields...))
	}
}

// Regression for #212: a postgres that drops the handshake or answers
// that it is starting up is an engine-unreachable failure; one that
// rejects the credentials is not.
func TestConnectClassifiesHandshakeFailures(t *testing.T) {
	cases := []struct {
		name   string
		handle func(net.Conn)
		reason reachability.Reason
	}{
		{"handshake dropped", fakeserver.Close, reachability.HandshakeDropped},
		{"starting up", answerStartupWith(sqlstateCannotConnectNow, "the database system is starting up"), reachability.Starting},
		{"authentication failed", answerStartupWith("28P01", "password authentication failed for user \"app\""), 0},
		{"unknown database", answerStartupWith("3D000", "database \"postgres\" does not exist"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port := fakeserver.Start(t, tc.handle)
			_, err := Connect(context.Background(), config.PostgresConn{Host: host, Port: port, User: "app"})
			if err == nil {
				t.Fatal("Connect succeeded against a server that never completes the handshake")
			}
			ue, unreachable := errors.AsType[*reachability.UnreachableError](err)
			if tc.reason == 0 {
				if unreachable {
					t.Errorf("Connect = %v, want a failure the engine reported, not engine-unreachable", err)
				}
				return
			}
			if !unreachable || ue.Reason != tc.reason {
				t.Errorf("Connect = %v, want an *UnreachableError with reason %s", err, tc.reason)
			}
		})
	}
}
