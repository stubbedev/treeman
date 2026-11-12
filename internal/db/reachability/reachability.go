// Package reachability probes a TCP host:port quickly so that an
// unreachable container surfaces as a clear error before the driver
// wraps it in driver-specific noise, and bounds the wait for an engine
// that is still starting.
package reachability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"syscall"
	"time"
)

// DefaultTimeout is the per-probe timeout. Short enough that a
// healthy local service connects in <10ms but long enough that a
// stale Docker network still answers with ECONNREFUSED before us
// giving up.
const DefaultTimeout = 1500 * time.Millisecond

// StartupWait bounds how long WaitFor keeps retrying an engine that is
// not ready yet under a ctx marked by WithStartupWait. It covers a
// container that is still booting when a worktree create starts,
// including an Elasticsearch cluster recovering its shards back to
// yellow.
const StartupWait = 2 * time.Minute

const pollInterval = 250 * time.Millisecond

// Reason is why an engine did not answer: each one is what an engine
// that is down or still starting looks like from treeman's side.
type Reason int

const (
	// Refused: the host answered, but nothing listens on the port.
	Refused Reason = iota + 1
	// TimedOut: the TCP connect timed out (the port is filtered, or
	// nothing listens yet behind a proxy that drops the SYN).
	TimedOut
	// ContainerDown: the engine's container is stopped or not created.
	ContainerDown
	// HandshakeDropped: TCP connected but the connection was refused,
	// reset or closed during the protocol handshake, as when docker's
	// userland proxy accepts before the engine listens.
	HandshakeDropped
	// Starting: the engine completed the handshake to say it does not
	// accept connections yet.
	Starting
)

func (r Reason) String() string {
	switch r {
	case Refused:
		return "tcp connect refused"
	case TimedOut:
		return "tcp connect timeout"
	case ContainerDown:
		return "container not running"
	case HandshakeDropped:
		return "connection dropped during the protocol handshake"
	case Starting:
		return "engine still starting"
	}
	return fmt.Sprintf("reason(%d)", int(r))
}

// UnreachableError is an engine that did not answer for a Reason that
// waiting can get past: the create pipeline waits for it at startup
// (WaitFor with IsUnreachable) and the daemon re-runs a create that
// failed on it once the engine answers. A failure the engine itself
// reports (bad credentials, an unknown database) is never one.
type UnreachableError struct {
	Engine string
	// Addr is the host:port dialled, or the container reference for
	// ContainerDown.
	Addr   string
	Reason Reason
	// Hint, when set, says what to check.
	Hint string
	// Err is the underlying failure; nil for TimedOut.
	Err error
}

func (e *UnreachableError) Error() string {
	msg := fmt.Sprintf("%s unreachable at %s (%s", e.Engine, e.Addr, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.Hint != "" {
		msg += " — " + e.Hint
	}
	return msg + ")"
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// IsUnreachable reports whether err is, or wraps, an engine that is
// down or still starting (an *UnreachableError).
func IsUnreachable(err error) bool {
	_, ok := errors.AsType[*UnreachableError](err)
	return ok
}

// ProbeCtx tries a single TCP connect bounded by ctx and DefaultTimeout,
// whichever ends first, and returns immediately on failure. A refusal
// or a connect timeout is an *UnreachableError. When ctx itself ends
// first the returned error wraps ctx.Err(), so a caller's cancellation
// or deadline is never reported as an unreachable service.
func ProbeCtx(ctx context.Context, engine, host string, port uint16) error {
	probeCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	addr := net.JoinHostPort(host, strconv.Itoa(int(port)))
	d := net.Dialer{}
	conn, err := d.DialContext(probeCtx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s probe of %s interrupted: %w", engine, addr, ctxErr)
	}
	if probeCtx.Err() != nil || isTimeout(err) {
		return &UnreachableError{Engine: engine, Addr: addr, Reason: TimedOut, Hint: fmt.Sprintf(
			"is the service running and the port exposed? if it's in docker, try `container:`, `compose_service:`, or publish the port with `-p HOST:%d`",
			port,
		)}
	}
	containerHint := "if the service is in docker, set `container:`/`compose_service:` so treeman can find its bridge IP or published port"
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%s unreachable at %s (tcp connect failed: %w — %s)", engine, addr, err, containerHint)
	}
	hint := containerHint
	if isLoopback(host) {
		hint = "nothing is listening there: the engine is down or still starting, check `treeman doctor`"
	}
	return &UnreachableError{Engine: engine, Addr: addr, Reason: Refused, Hint: hint, Err: err}
}

// ProbeURLCtx parses a URL-style connection string and probes the
// resulting host:port once, like ProbeCtx.
func ProbeURLCtx(ctx context.Context, engine, raw string) error {
	host, port, err := HostPort(engine, raw)
	if err != nil {
		return err
	}
	return ProbeCtx(ctx, engine, host, port)
}

// HandshakeError classifies err, the failure of a driver's protocol
// handshake with engine at addr under ctx. A transport failure is an
// *UnreachableError: the connect refused or timed out, or the
// connection was reset or closed before the handshake completed
// (io.EOF, ECONNRESET, EPIPE, or one of the driver's own lost-connection
// sentinels in lost). Any other error, notably one the engine reported
// (authentication, an unknown database), and any failure after ctx
// ended, is returned unchanged.
func HandshakeError(ctx context.Context, engine, addr string, err error, lost ...error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	var reason Reason
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		reason = Refused
	case connectionLost(err, lost):
		reason = HandshakeDropped
	case isTimeout(err):
		reason = TimedOut
	default:
		return err
	}
	return &UnreachableError{Engine: engine, Addr: addr, Reason: reason, Err: err}
}

// StartingError is engine at addr answering its handshake with err, a
// refusal that says it is still starting (postgres 57P03, redis
// LOADING, an Elasticsearch cluster below yellow health).
func StartingError(engine, addr string, err error) *UnreachableError {
	return &UnreachableError{Engine: engine, Addr: addr, Reason: Starting, Err: err}
}

// droppedConnection are the transport errors of a connection the peer
// closed or reset.
var droppedConnection = [...]error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.EPIPE, syscall.ECONNABORTED}

func connectionLost(err error, lost []error) bool {
	for _, target := range droppedConnection {
		if errors.Is(err, target) {
			return true
		}
	}
	for _, target := range lost {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

type startupWaitKey struct{}

// WithStartupWait returns ctx marked so that WaitFor under it keeps
// retrying an engine that is down or still starting, for up to
// StartupWait. Set it where a delay beats a failure, such as the
// worktree finalize pipeline; diagnostics leave it unset to fail fast.
func WithStartupWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, startupWaitKey{}, true)
}

// StartupWaitEnabled reports whether ctx was marked by WithStartupWait.
func StartupWaitEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(startupWaitKey{}).(bool)
	return enabled
}

// WaitFor runs attempt once, or, when ctx is marked by WithStartupWait,
// until it succeeds, returns an error retryable rejects, or StartupWait
// (or ctx's earlier deadline) elapses, pausing between attempts. When
// the wait ends without success the returned error wraps both the
// context's error and the last retryable error, with how long was
// waited.
func WaitFor(ctx context.Context, attempt func(context.Context) error, retryable func(error) bool) error {
	if !StartupWaitEnabled(ctx) {
		return attempt(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, StartupWait)
	defer cancel()
	start := time.Now()
	var last error
	for {
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil && last != nil {
			return gaveUp(ctx, start, last)
		}
		if !retryable(err) {
			return err
		}
		last = err
		select {
		case <-ctx.Done():
			return gaveUp(ctx, start, last)
		case <-time.After(pollInterval):
		}
	}
}

func gaveUp(ctx context.Context, start time.Time, last error) error {
	return fmt.Errorf("still not ready after waiting %s (%w): %w",
		time.Since(start).Round(100*time.Millisecond), ctx.Err(), last)
}

// HostPort extracts host and port from a URL-style connection string,
// falling back to the scheme's default port when the URL omits one.
func HostPort(engine, raw string) (string, uint16, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, fmt.Errorf("%s: invalid URI %q: %w", engine, raw, err)
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("%s: no host in URI %q", engine, raw)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return "", 0, fmt.Errorf("%s: bad port in URI %q: %w", engine, raw, err)
		}
		return host, uint16(n), nil
	}
	if dp, ok := defaultPort(u.Scheme); ok {
		return host, dp, nil
	}
	return "", 0, fmt.Errorf("%s: no port in URI %q and no default for scheme %q", engine, raw, u.Scheme)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func defaultPort(scheme string) (uint16, bool) {
	switch scheme {
	case "http":
		return 80, true
	case "https":
		return 443, true
	case "redis", "rediss":
		return 6379, true
	case "mongodb", "mongodb+srv":
		return 27017, true
	case "mysql":
		return 3306, true
	case "postgres", "postgresql":
		return 5432, true
	}
	return 0, false
}
