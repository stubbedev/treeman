// Package containerip resolves connectivity details for a named
// container (docker / podman / nerdctl / finch), so
// treeman can dial DB services that run inside a container with no
// published port, behind a compose service name, or from inside a
// devcontainer that shares a network with the database.
//
// Resolution prefers a published host port mapping (works on macOS
// and Windows Docker Desktop where bridge IPs aren't routable) and
// falls back to the container's bridge-network IP (Linux) when no
// host port is published. When treeman itself runs inside a
// container and the engine socket isn't reachable, lookups are
// skipped so the caller falls through to the configured Host —
// which on a shared compose network is the sibling service name.
//
// Resolution is cached per (engine, key) for a TTL so a hot path
// doesn't shell out on every connection open. The cache is
// invalidated by Refresh when a caller observes a connection
// failure that might indicate the container restarted.
package containerip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CacheTTL is how long a successful lookup is cached. Long enough
// to amortise the inspect cost across a worktree-create burst;
// short enough that a container restart settles quickly.
var CacheTTL = 30 * time.Second

// HostDockerInternal is the Docker Desktop / Podman Desktop /
// OrbStack alias that resolves to the host-loopback gateway from
// inside a container. Used as a fallback when treeman runs inside
// a container and the configured host is unreachable.
const HostDockerInternal = "host.docker.internal"

// Opts describes how to resolve a container/compose reference into
// a (host, port) endpoint. All fields are optional; an empty Opts
// resolves to nil endpoint with no error.
type Opts struct {
	// Container is a raw container name or ID (looked up via
	// `<engine> inspect`). When set, ComposeService is ignored.
	Container string
	// ComposeService is a docker-compose service name. Resolved by
	// matching the standard compose labels (project + service) and
	// taking the first running container.
	ComposeService string
	// ComposeProject is the compose project name to scope the
	// service lookup to. Defaults to COMPOSE_PROJECT_NAME if unset.
	ComposeProject string
	// Engine is the container engine binary (docker, podman,
	// nerdctl, finch). Default "docker".
	Engine string
	// Network is the preferred docker network name when the
	// container is attached to several. Resolution picks this
	// network's IP first, then falls back to the deterministic
	// "first non-bridge network, alphabetically" rule.
	Network string
	// InternalPort is the in-container port the caller wants to
	// reach. When set, ResolveAddr prefers a published host-port
	// mapping for this port over the bridge IP — required on
	// macOS / Windows Docker Desktop.
	InternalPort uint16
}

// Addr is a resolved (host, port) pair. Port == 0 means the caller
// should keep its configured port.
type Addr struct {
	Host string
	Port uint16
}

type entry struct {
	addr   Addr
	expiry time.Time
}

var (
	mu    sync.Mutex
	cache = map[string]entry{}
)

func cacheKey(opts Opts) string {
	return opts.normEngine() + "/" + opts.Container + "/" + opts.ComposeProject + "/" + opts.ComposeService + "/" + opts.Network + "/" + strconv.Itoa(
		int(opts.InternalPort),
	)
}

func (o Opts) normEngine() string {
	if o.Engine == "" {
		return "docker"
	}
	return o.Engine
}

func (o Opts) empty() bool {
	return o.Container == "" && o.ComposeService == ""
}

// ResolveAddr returns the host+port to dial for opts. Returns
// (nil, nil) when opts has no container/service reference — the
// caller should use its configured Host/Port as-is.
//
// Lookup order:
//  1. cached result for opts.
//  2. If InsideContainer() and no engine socket is reachable: nil
//     (sibling DNS handles it).
//  3. `<engine> inspect` against the resolved container id:
//     - prefer a published host port mapping for InternalPort
//     (127.0.0.1:HOST_PORT) — works cross-platform.
//     - else use the preferred network's IPAddress + InternalPort.
//  4. When inspect fails AND we're inside a container, fall back
//     to host.docker.internal at InternalPort.
func ResolveAddr(opts Opts) (*Addr, error) {
	if opts.empty() {
		return nil, nil
	}
	key := cacheKey(opts)

	mu.Lock()
	if e, ok := cache[key]; ok && time.Now().Before(e.expiry) {
		mu.Unlock()
		return &e.addr, nil
	}
	mu.Unlock()

	addr, err := resolveUncached(opts)
	if err != nil {
		// Last-resort fallback for treeman-in-container: try the
		// host loopback alias before bubbling the error up.
		if InsideContainer() && opts.InternalPort != 0 && hostLoopbackReachable(opts.InternalPort) {
			a := Addr{Host: HostDockerInternal, Port: opts.InternalPort}
			mu.Lock()
			cache[key] = entry{addr: a, expiry: time.Now().Add(CacheTTL)}
			mu.Unlock()
			return &a, nil
		}
		return nil, err
	}
	if addr == nil {
		return nil, nil
	}
	mu.Lock()
	cache[key] = entry{addr: *addr, expiry: time.Now().Add(CacheTTL)}
	mu.Unlock()
	return addr, nil
}

func resolveUncached(opts Opts) (*Addr, error) {
	engine := opts.normEngine()
	if InsideContainer() && !engineSocketReachable(engine) {
		// Skip the inspect path entirely — let the caller's
		// configured host (sibling compose DNS) handle it.
		return nil, nil
	}
	id, err := resolveContainerID(opts)
	if err != nil {
		return nil, err
	}
	info, err := inspectContainer(engine, id)
	if err != nil {
		return nil, err
	}
	// Prefer a published-port mapping when the caller asked about
	// a specific internal port. Cross-platform: bridge IPs are
	// not routable from macOS/Windows host, but localhost:HOST_PORT
	// always is.
	if opts.InternalPort != 0 {
		if hp, ok := info.publishedHostPort(opts.InternalPort); ok {
			return &Addr{Host: "127.0.0.1", Port: hp}, nil
		}
	}
	// Fall back to the bridge-network IP.
	ip, err := info.pickIP(opts.Network)
	if err != nil {
		return nil, err
	}
	if ip == "" {
		return nil, fmt.Errorf("%s inspect %s: no IP found (is the container running?)", engine, id)
	}
	return &Addr{Host: ip, Port: opts.InternalPort}, nil
}

// Resolve is the legacy single-IP entry point. Kept for callers
// that don't want the full Addr shape (e.g. URI rewrites that only
// substitute the host part). Returns the empty string for empty
// `container` so callers can fall through to their configured Host.
func Resolve(container, engine string) (string, error) {
	if container == "" {
		return "", nil
	}
	addr, err := ResolveAddr(Opts{Container: container, Engine: engine})
	if err != nil {
		return "", err
	}
	if addr == nil {
		return "", nil
	}
	return addr.Host, nil
}

// Refresh evicts cached entries that match opts so the next call
// re-issues `inspect`. Called on connection-failure paths so a
// restarted container with a new IP settles within one retry.
func Refresh(container, engine string) {
	if container == "" {
		return
	}
	if engine == "" {
		engine = "docker"
	}
	prefix := engine + "/" + container + "/"
	mu.Lock()
	for k := range cache {
		if strings.HasPrefix(k, prefix) {
			delete(cache, k)
		}
	}
	mu.Unlock()
}

// RefreshOpts evicts the exact-Opts cache entry.
func RefreshOpts(opts Opts) {
	mu.Lock()
	delete(cache, cacheKey(opts))
	mu.Unlock()
}

// ContainerID returns the container id/name for opts. For a
// Container ref this is the ref itself; for a ComposeService it
// runs `<engine> ps -q --filter label=...` to find the running
// container.
func ContainerID(opts Opts) (string, error) {
	return resolveContainerID(opts)
}

// resolveContainerID is the internal alias kept for older callers.
func resolveContainerID(opts Opts) (string, error) {
	if opts.Container != "" {
		return opts.Container, nil
	}
	project := opts.ComposeProject
	if project == "" {
		project = os.Getenv("COMPOSE_PROJECT_NAME")
	}
	engine := opts.normEngine()
	args := []string{
		"ps", "-q", "--filter", "status=running",
		"--filter", "label=com.docker.compose.service=" + opts.ComposeService,
	}
	if project != "" {
		args = append(args, "--filter", "label=com.docker.compose.project="+project)
	}
	out, err := exec.CommandContext(context.Background(), engine, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s ps -q (service=%s, project=%s): %w", engine, opts.ComposeService, project, err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line, nil
		}
	}
	return "", fmt.Errorf("no running container for compose service %q (project=%q) via %s", opts.ComposeService, project, engine)
}

// inspectInfo is the subset of `<engine> inspect` JSON treeman uses.
type inspectInfo struct {
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Networks  map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
	Config struct {
		Env []string `json:"Env"`
	} `json:"Config"`
}

func inspectContainer(engine, id string) (*inspectInfo, error) {
	out, err := exec.CommandContext(context.Background(), engine, "inspect", id).Output()
	if err != nil {
		return nil, fmt.Errorf("%s inspect %s: %w", engine, id, err)
	}
	var arr []inspectInfo
	if err := json.Unmarshal(out, &arr); err != nil {
		return nil, fmt.Errorf("%s inspect %s: parse json: %w", engine, id, err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("%s inspect %s: no result", engine, id)
	}
	return &arr[0], nil
}

// pickIP returns the IPAddress for the requested network when
// preferred != "", else the first non-empty IP from the Networks
// map iterated in deterministic order (non-"bridge" networks
// first, sorted alphabetically), else the legacy IPAddress field.
func (i *inspectInfo) pickIP(preferred string) (string, error) {
	if preferred != "" {
		if n, ok := i.NetworkSettings.Networks[preferred]; ok && n.IPAddress != "" {
			return n.IPAddress, nil
		}
		return "", fmt.Errorf("container has no IP on network %q", preferred)
	}
	names := make([]string, 0, len(i.NetworkSettings.Networks))
	for n := range i.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Slice(names, func(a, b int) bool {
		// Push the default "bridge" network last so compose-managed
		// custom networks win when both are present.
		ai, bi := names[a] == "bridge", names[b] == "bridge"
		if ai != bi {
			return !ai
		}
		return names[a] < names[b]
	})
	for _, n := range names {
		if ip := i.NetworkSettings.Networks[n].IPAddress; ip != "" {
			return ip, nil
		}
	}
	return i.NetworkSettings.IPAddress, nil
}

// publishedHostPort returns the host port that maps to internalPort
// (the container-side port). Returns false when no mapping exists.
// Prefers a mapping bound to all interfaces (0.0.0.0 / ::); falls
// back to any binding when only loopback is exposed.
func (i *inspectInfo) publishedHostPort(internalPort uint16) (uint16, bool) {
	if len(i.NetworkSettings.Ports) == 0 {
		return 0, false
	}
	want := strconv.Itoa(int(internalPort)) + "/"
	// Two passes: prefer 0.0.0.0/:: bindings over loopback-only.
	for _, pass := range []bool{true, false} {
		for key, binds := range i.NetworkSettings.Ports {
			if !strings.HasPrefix(key, want) {
				continue
			}
			for _, b := range binds {
				public := b.HostIP == "" || b.HostIP == "0.0.0.0" || b.HostIP == "::"
				if pass != public {
					continue
				}
				p, err := strconv.ParseUint(b.HostPort, 10, 16)
				if err != nil {
					continue
				}
				return uint16(p), true
			}
		}
	}
	return 0, false
}

// EnvLookup runs `<engine> inspect` and returns Config.Env as a
// map. Empty map + nil error when opts has no container ref or
// inspect fails (callers treat this as "no autodiscovery
// available", not a fatal error).
func EnvLookup(opts Opts) (map[string]string, error) {
	if opts.empty() {
		return nil, nil
	}
	engine := opts.normEngine()
	id, err := resolveContainerID(opts)
	if err != nil {
		return nil, err
	}
	info, err := inspectContainer(engine, id)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(info.Config.Env))
	for _, kv := range info.Config.Env {
		if before, after, ok := strings.Cut(kv, "="); ok {
			out[before] = after
		}
	}
	return out, nil
}

// hostLoopbackReachable probes host.docker.internal:port quickly
// from inside a container. Used only after the primary lookup
// failed — short timeout, no caching.
func hostLoopbackReachable(port uint16) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(HostDockerInternal, strconv.Itoa(int(port))))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// RewriteHostPortInURI returns `uri` with its host swapped for
// `newHost`. Preserves scheme, userinfo, port, path, query.
// When newPort != 0 the URI's port is also rewritten.
func RewriteHostPortInURI(uri, newHost string) string {
	return RewriteHostPortInURIWithPort(uri, newHost, 0)
}

// RewriteHostPortInURIWithPort rewrites both host and (when non-zero) port.
func RewriteHostPortInURIWithPort(uri, newHost string, newPort uint16) string {
	scheme, rest := splitScheme(uri)
	if scheme == "" {
		return uri
	}
	userinfo, hostPath := splitUserinfo(rest)
	hostPart, tail := splitHostFromTail(hostPath)
	if hostPart == "" {
		return uri
	}
	port := splitHostPort(hostPart)
	newHostPort := newHost
	switch {
	case newPort != 0:
		newHostPort = newHost + ":" + strconv.Itoa(int(newPort))
	case port != "":
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
	before, after, ok := strings.Cut(uri, "://")
	if !ok {
		return "", uri
	}
	return before, after
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

// URIPort extracts the port from a URI, returning fallback when
// the URI is malformed or omits a port.
func URIPort(uri string, fallback uint16) uint16 {
	_, rest := splitScheme(uri)
	if rest == "" {
		return fallback
	}
	_, hostPath := splitUserinfo(rest)
	hostPart, _ := splitHostFromTail(hostPath)
	p := splitHostPort(hostPart)
	if p == "" {
		return fallback
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return fallback
	}
	return uint16(n)
}

func splitHostPort(s string) (port string) {
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i+1:], "]") {
		return s[i+1:]
	}
	return ""
}

// ErrNoContainer is returned when Resolve is called with no
// container name. Callers can ignore it and fall through to using
// their configured Host.
var ErrNoContainer = errors.New("no container configured")
