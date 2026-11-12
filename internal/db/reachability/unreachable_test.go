package reachability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
)

func isReason(err error, want Reason) bool {
	ue, ok := errors.AsType[*UnreachableError](err)
	return ok && ue.Reason == want
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}

var errDriverLost = errors.New("driver: lost connection")

// Regression for #212: a handshake the engine never got to answer is
// an engine-unreachable failure with its reason; one the engine
// answered (bad credentials, an unknown database) and one cut short by
// the caller's own context are not.
func TestHandshakeErrorClassifiesTransportFailures(t *testing.T) {
	live := context.Background()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name   string
		ctx    context.Context
		err    error
		reason Reason
	}{
		{"connection closed", live, fmt.Errorf("read: %w", io.EOF), HandshakeDropped},
		{"connection closed mid-packet", live, io.ErrUnexpectedEOF, HandshakeDropped},
		{"connection reset", live, &net.OpError{Op: "read", Err: syscall.ECONNRESET}, HandshakeDropped},
		{"driver sentinel", live, fmt.Errorf("ping: %w", errDriverLost), HandshakeDropped},
		{"connect refused", live, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, Refused},
		{"connect timeout", live, &net.OpError{Op: "dial", Err: timeoutError{}}, TimedOut},
		{"authentication failed", live, errors.New("Error 1045 (28000): Access denied for user 'app'"), 0},
		{"caller's context ended", ended, io.EOF, 0},
	}
	for _, tc := range cases {
		got := HandshakeError(tc.ctx, "mysql", "127.0.0.1:3306", tc.err, errDriverLost)
		if tc.reason == 0 {
			if got != tc.err { //nolint:errorlint // the error must come back as is, not wrapped
				t.Errorf("%s: HandshakeError = %v, want the error unchanged", tc.name, got)
			}
			continue
		}
		if !isReason(got, tc.reason) {
			t.Errorf("%s: HandshakeError = %v, want an *UnreachableError with reason %s", tc.name, got, tc.reason)
		}
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: HandshakeError = %v, want it to wrap %v", tc.name, got, tc.err)
		}
	}
}

// A connect timeout is an engine-unreachable failure, typed rather
// than recognised by its message.
func TestProbeCtxTimeoutIsUnreachable(t *testing.T) {
	err := ProbeCtx(context.Background(), "mysql", "198.51.100.1", 3306)
	if err == nil {
		t.Fatal("expected probe to fail against unroutable host")
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		t.Skipf("dial to the unroutable host failed without timing out: %v", err)
	}
	if !isReason(err, TimedOut) {
		t.Errorf("err = %v, want a timed-out *UnreachableError", err)
	}
}
