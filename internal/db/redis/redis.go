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
	"github.com/stubbedev/treeman/internal/db/adapter"
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

// Connect probes TCP reachability, parses the URL and completes one
// PING on the base client. A PING the server never got to answer (the
// connection refused, reset or closed), or answered with LOADING, is
// an engine-unreachable failure. Per-db clients are opened lazily.
func Connect(ctx context.Context, cfg config.RedisConn) (*Driver, error) {
	opts := containerip.FromRef(cfg.ContainerRef, containerip.URIPort(cfg.URL, 6379))
	return adapter.ConnectURL(ctx, "redis", opts, cfg.URL, func(ctx context.Context, url, addr string) (*Driver, error) {
		o, err := redis.ParseURL(url)
		if err != nil {
			return nil, fmt.Errorf("redis url: %w", err)
		}
		if cfg.PoolMax > 0 {
			o.PoolSize = int(cfg.PoolMax)
		}
		base := redis.NewClient(o)
		if err := base.Ping(ctx).Err(); err != nil {
			_ = base.Close()
			if redis.IsLoadingError(err) {
				return nil, fmt.Errorf("ping redis: %w", reachability.StartingError("redis", addr, err))
			}
			return nil, fmt.Errorf("ping redis: %w", reachability.HandshakeError(ctx, "redis", addr, err))
		}
		return &Driver{baseOpts: o, clients: map[int]*redis.Client{-1: base}}, nil
	})
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
	for line := range strings.SplitSeq(info, "\n") {
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
