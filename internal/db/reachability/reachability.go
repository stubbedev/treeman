// Package reachability probes a TCP host:port quickly so that an
// unreachable container surfaces as a clear error before the driver
// wraps it in driver-specific noise.
package reachability

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// DefaultTimeout is the per-probe timeout. Short enough that a
// healthy local service connects in <10ms but long enough that a
// stale Docker network still answers with ECONNREFUSED before us
// giving up.
const DefaultTimeout = 1500 * time.Millisecond

// Probe tries a TCP connect within DefaultTimeout. On failure
// returns an error whose message names the engine so callers don't
// have to wrap it.
func Probe(engine, host string, port uint16) error {
	return ProbeWithTimeout(engine, host, port, DefaultTimeout)
}

// ProbeWithTimeout is the explicit-timeout variant.
func ProbeWithTimeout(engine, host string, port uint16, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf(
				"%s unreachable at %s:%d (tcp connect timeout after %dms — is the service running and the port exposed? if it's in docker, try `container:`, `compose_service:`, or publish the port with `-p HOST:%d`)",
				engine, host, port, timeout.Milliseconds(), port)
		}
		return fmt.Errorf("%s unreachable at %s:%d (tcp connect refused: %v — if the service is in docker, set `container:`/`compose_service:` so treeman can find its bridge IP or published port)", engine, host, port, err)
	}
	_ = conn.Close()
	return nil
}

// ProbeURL parses a URL-style connection string and probes the
// resulting host:port. Falls back to scheme default if the URL
// omits a port.
func ProbeURL(engine, raw string) error {
	host, port, err := parseHostPort(engine, raw)
	if err != nil {
		return err
	}
	return Probe(engine, host, port)
}

func parseHostPort(engine, raw string) (string, uint16, error) {
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
