// Package redis is the treeman Redis driver. The treeman scoping
// scheme reuses db indices (0..15) per worktree.
package redis

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Driver wraps go-redis's Client. Clients are pooled per db index
// because FlushDB / inspection tools each need a connection scoped
// to a specific db, and go-redis treats Options.DB as fixed-per-client.
type Driver struct {
	baseOpts *redis.Options

	mu      sync.Mutex
	clients map[int]*redis.Client // keyed by db index; -1 means base

	// Lazy-resolved COPY-command support flag. Redis 6.2+ supports
	// the server-side `COPY` command (fast, single round-trip per
	// key); older versions need the DUMP+RESTORE fallback. The
	// detection runs once on the first snapshot op and caches.
	copyOnce      sync.Once
	copySupported bool
}

// Connect probes TCP reachability + parses the URL once. Per-db
// clients are opened lazily.
func Connect(ctx context.Context, cfg config.RedisConn) (*Driver, error) {
	url := cfg.URL
	if cfg.Container != "" || cfg.ComposeService != "" {
		opts := containerip.Opts{
			Container:      cfg.Container,
			ComposeService: cfg.ComposeService,
			ComposeProject: cfg.ComposeProject,
			Engine:         cfg.ContainerEngine,
			Network:        cfg.Network,
			InternalPort:   containerip.URIPort(url, 6379),
		}
		addr, err := containerip.ResolveAddr(opts)
		if err != nil {
			return nil, fmt.Errorf("resolve container: %w", err)
		}
		if addr != nil {
			url = containerip.RewriteHostPortInURIWithPort(url, addr.Host, addr.Port)
		}
	}
	if err := reachability.ProbeURLCtx(ctx, "redis", url); err != nil {
		return nil, err
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	if cfg.PoolMax > 0 {
		opts.PoolSize = int(cfg.PoolMax)
	}
	return &Driver{baseOpts: opts, clients: map[int]*redis.Client{}}, nil
}

// Close shuts down every pooled client.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var firstErr error
	for k, c := range d.clients {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(d.clients, k)
	}
	return firstErr
}

// clientFor returns a pooled redis.Client scoped to db. The first
// call for a given db opens the client; subsequent calls reuse it.
// Pass -1 for "use whatever DB was encoded in the URL" (base).
func (d *Driver) clientFor(db int) *redis.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.clients[db]; ok {
		return c
	}
	o := *d.baseOpts // shallow copy; we only override DB
	if db >= 0 {
		o.DB = db
	}
	c := redis.NewClient(&o)
	d.clients[db] = c
	return c
}

// EngineVersion → `INFO server` → `redis_version:` line.
func (d *Driver) EngineVersion(ctx context.Context) (string, error) {
	info, err := d.clientFor(-1).Info(ctx, "server").Result()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimRight(line, "\r")
		if v, ok := strings.CutPrefix(line, "redis_version:"); ok {
			return v, nil
		}
	}
	return "", nil
}

// FlushDB issues `FLUSHDB` against the pooled client for `db`.
func (d *Driver) FlushDB(ctx context.Context, db uint8) error {
	if err := d.clientFor(int(db)).FlushDB(ctx).Err(); err != nil {
		return fmt.Errorf("FLUSHDB %d: %w", db, err)
	}
	return nil
}
