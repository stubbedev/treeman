// Package fakeserver runs a loopback TCP server whose every connection
// a test scripts, to drive a database driver's handshake against an
// engine that drops or refuses it.
package fakeserver

import (
	"net"
	"strconv"
	"testing"
)

// Start listens on a loopback port and runs handle on every accepted
// connection, closing it afterwards. The listener closes when the test
// ends.
func Start(tb testing.TB, handle func(net.Conn)) (host string, port uint16) {
	tb.Helper()
	l, err := (&net.ListenConfig{}).Listen(tb.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				handle(c)
			}()
		}
	}()
	h, p, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		tb.Fatal(err)
	}
	return h, uint16(n)
}

// Close is a handler that closes the connection at once, as docker's
// userland proxy does when nothing listens behind it yet.
func Close(net.Conn) {}
