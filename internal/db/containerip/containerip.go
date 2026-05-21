// Package containerip resolves the bridge-network IP of a named
// container (docker / podman) so treeman can dial DB services that
// run inside a container with no published port. Sidesteps the need
// for `-p HOST:CONTAINER` port-publishing during dev — the IP lives
// on the docker bridge network which the host can route to directly
// on Linux (and via `host.docker.internal` on macOS / Windows
// Docker Desktop).
//
// Resolution is cached per (engine, container) for a TTL so a hot
// path doesn't shell out on every connection open. The cache is
// invalidated by Refresh(container) when a caller observes a
// connection failure that might indicate the container restarted
// with a new IP.
package containerip

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CacheTTL is how long a successful lookup is cached. Long enough
// to amortise the inspect cost across a worktree-create burst;
// short enough that a container restart settles quickly.
var CacheTTL = 30 * time.Second

type entry struct {
	ip     string
	expiry time.Time
}

var (
	mu    sync.Mutex
	cache = map[string]entry{}
)

// Resolve returns the bridge-network IPv4 address of `container`
// using `engine` (defaults to "docker"). Returns the empty string +
// nil when `container` is empty — callers fall through to using the
// configured Host.
func Resolve(container, engine string) (string, error) {
	if container == "" {
		return "", nil
	}
	if engine == "" {
		engine = "docker"
	}
	key := engine + "/" + container

	mu.Lock()
	if e, ok := cache[key]; ok && time.Now().Before(e.expiry) {
		mu.Unlock()
		return e.ip, nil
	}
	mu.Unlock()

	ip, err := inspectIP(engine, container)
	if err != nil {
		return "", err
	}
	mu.Lock()
	cache[key] = entry{ip: ip, expiry: time.Now().Add(CacheTTL)}
	mu.Unlock()
	return ip, nil
}

// Refresh evicts the cache entry for (engine, container) so the
// next Resolve issues a fresh inspect. Called on connection-failure
// paths so a restarted container with a new IP settles within one
// retry instead of one CacheTTL.
func Refresh(container, engine string) {
	if container == "" {
		return
	}
	if engine == "" {
		engine = "docker"
	}
	key := engine + "/" + container
	mu.Lock()
	delete(cache, key)
	mu.Unlock()
}

// inspectIP runs `<engine> inspect --format <template>` and returns
// the first non-empty IP it finds across the container's networks.
// Tries the legacy `.NetworkSettings.IPAddress` first (single-
// network containers / `--network bridge`) then walks
// `.NetworkSettings.Networks` for explicit network names (docker
// compose creates custom networks).
func inspectIP(engine, container string) (string, error) {
	// Walk Networks map first: handles custom networks correctly.
	tmpl := `{{range $k, $v := .NetworkSettings.Networks}}{{$v.IPAddress}}{{"\n"}}{{end}}`
	out, err := exec.Command(engine, "inspect", "--format", tmpl, container).Output()
	if err != nil {
		// Fall back to a single IPAddress field for older docker.
		out, err = exec.Command(engine, "inspect", "--format", `{{.NetworkSettings.IPAddress}}`, container).Output()
		if err != nil {
			return "", fmt.Errorf("%s inspect %s: %w", engine, container, err)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return line, nil
	}
	return "", fmt.Errorf("%s inspect %s: no IP found (is the container running?)", engine, container)
}

// RewriteHostPortInURI returns `uri` with its host swapped for
// `newHost`. Preserves scheme, userinfo, port, path, query.
// Returns the original URI on parse failure (caller may still
// dial successfully if their URI didn't actually need a rewrite).
func RewriteHostPortInURI(uri, newHost string) string {
	scheme, rest := splitScheme(uri)
	if scheme == "" {
		return uri
	}
	userinfo, hostPath := splitUserinfo(rest)
	hostPart, tail := splitHostFromTail(hostPath)
	if hostPart == "" {
		return uri
	}
	_, port := splitHostPort(hostPart)
	newHostPort := newHost
	if port != "" {
		newHostPort = newHost + ":" + port
	}
	out := scheme + "://"
	if userinfo != "" {
		out += userinfo + "@"
	}
	out += newHostPort + tail
	return out
}

func splitScheme(uri string) (scheme, rest string) {
	i := strings.Index(uri, "://")
	if i < 0 {
		return "", uri
	}
	return uri[:i], uri[i+3:]
}

func splitUserinfo(s string) (userinfo, rest string) {
	at := strings.Index(s, "@")
	slash := strings.Index(s, "/")
	if at >= 0 && (slash < 0 || at < slash) {
		return s[:at], s[at+1:]
	}
	return "", s
}

func splitHostFromTail(s string) (host, tail string) {
	for i, c := range s {
		if c == '/' || c == '?' || c == '#' {
			return s[:i], s[i:]
		}
	}
	return s, ""
}

func splitHostPort(s string) (host, port string) {
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i+1:], "]") {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// ErrNoContainer is returned when Resolve is called with no
// container name. Callers can ignore it and fall through to their
// configured Host.
var ErrNoContainer = errors.New("no container configured")
