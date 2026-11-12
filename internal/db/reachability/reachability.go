// Package reachability probes a TCP host:port quickly so that an
// unreachable container surfaces as a clear error before the driver
// wraps it in driver-specific noise, and bounds the wait for an engine
// that is still starting.
package reachability

import (
	"context"
	"errors"
	"fmt"
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

// ProbeCtx tries a single TCP connect bounded by ctx and DefaultTimeout,
// whichever ends first, and returns immediately on failure. When ctx
// itself ends first the returned error wraps ctx.Err(), so a caller's
// cancellation or deadline is never reported as an unreachable service.
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
	if probeCtx.Err() != nil {
		return fmt.Errorf(
			"%s unreachable at %s (tcp connect timeout — is the service running and the port exposed? if it's in docker, try `container:`, `compose_service:`, or publish the port with `-p HOST:%d`)",
			engine,
			addr,
			port,
		)
	}
	outcome, hint := "failed", "if the service is in docker, set `container:`/`compose_service:` so treeman can find its bridge IP or published port"
	if IsRefused(err) {
		outcome = "refused"
		if isLoopback(host) {
			hint = "nothing is listening there: the engine is down or still starting, check `treeman doctor`"
		}
	}
	return fmt.Errorf("%s unreachable at %s (tcp connect %s: %w — %s)", engine, addr, outcome, err, hint)
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

// IsRefused reports whether err is a TCP connect refusal: the host
// answered but nothing listens on the port, which is what an engine
// that is down or still starting looks like.
func IsRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
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
