// Package redis is the treeman Redis driver. The treeman scoping
// scheme reuses db indices (0..15) per worktree.
package redis

import (
	"context"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Driver wraps go-redis's Client. We keep the parsed URL around so
// FlushDB can dial a per-index sub-client.
type Driver struct {
	url string

	// Lazy-resolved COPY-command support flag. Redis 6.2+ supports
	// the server-side `COPY` command (fast, single round-trip per
	// key); older versions need the DUMP+RESTORE fallback. The
	// detection runs once on the first snapshot op and caches.
	copyOnce      sync.Once
	copySupported bool
}

// Connect probes TCP reachability + returns a Driver. The actual
// client is opened lazily per-index since FLUSHDB needs a connection
// scoped to a specific db index.
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
	if _, err := redis.ParseURL(url); err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	return &Driver{url: url}, nil
}

// Close is a no-op — each FLUSHDB opens its own short-lived client.
func (d *Driver) Close() error { return nil }

// EngineVersion → `INFO server` → `redis_version:` line.
func (d *Driver) EngineVersion(ctx context.Context) (string, error) {
	opts, _ := redis.ParseURL(d.url)
	c := redis.NewClient(opts)
	defer c.Close()
	info, err := c.Info(ctx, "server").Result()
	if err != nil {
		return "", err
	}
	for _, line := range splitLines(info) {
		if v, ok := stripPrefix(line, "redis_version:"); ok {
			return v, nil
		}
	}
	return "", nil
}

// FlushDB issues `SELECT <db>` then `FLUSHDB`.
func (d *Driver) FlushDB(ctx context.Context, db uint8) error {
	opts, err := redis.ParseURL(d.url)
	if err != nil {
		return err
	}
	opts.DB = int(db)
	c := redis.NewClient(opts)
	defer c.Close()
	if err := c.FlushDB(ctx).Err(); err != nil {
		return fmt.Errorf("FLUSHDB %d: %w", db, err)
	}
	return nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func stripPrefix(line, prefix string) (string, bool) {
	if len(line) < len(prefix) {
		return "", false
	}
	if line[:len(prefix)] != prefix {
		return "", false
	}
	return line[len(prefix):], true
}
