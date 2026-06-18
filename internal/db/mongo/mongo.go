// Package mongo is the treeman MongoDB driver. Uses
// go.mongodb.org/mongo-driver v2.
package mongo

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Driver wraps the mongo Client.
type Driver struct {
	Client *mongo.Client
	cfg    config.MongoConn

	// dumpToolsOnce caches whether mongodump+mongorestore exist in the
	// configured container, so the dump/restore fast path is probed
	// once rather than per clone.
	dumpToolsOnce sync.Once
	dumpToolsOK   bool

	// archives caches a per-template `mongodump --archive` so a fan-out
	// dumps the (immutable, fingerprint-named) template once and each
	// clone restores from that archive. Keyed by template DB name →
	// *mongoArchive. See dumprestore.go.
	archives sync.Map
}

// Connect parses cfg.URI, probes TCP reachability (when the URI
// isn't mongodb+srv:// where the host is a SRV record), and dials.
func Connect(ctx context.Context, cfg config.MongoConn) (*Driver, error) {
	uri := cfg.URI
	if cfg.Container != "" || cfg.ComposeService != "" {
		opts := containerip.Opts{
			Container:      cfg.Container,
			ComposeService: cfg.ComposeService,
			ComposeProject: cfg.ComposeProject,
			Engine:         cfg.ContainerEngine,
			Network:        cfg.Network,
			InternalPort:   containerip.URIPort(uri, 27017),
		}
		addr, err := containerip.ResolveAddr(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("resolve container: %w", err)
		}
		if addr != nil {
			uri = containerip.RewriteHostPortInURIWithPort(uri, addr.Host, addr.Port)
		}
	}
	if !strings.HasPrefix(uri, "mongodb+srv://") {
		if err := reachability.ProbeURLCtx(ctx, "mongodb", uri); err != nil {
			return nil, err
		}
	}
	clientOpts := options.Client().ApplyURI(uri)
	if cfg.PoolMax > 0 {
		clientOpts.SetMaxPoolSize(uint64(cfg.PoolMax))
	}
	c, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	if err := c.Ping(ctx, nil); err != nil {
		_ = c.Disconnect(ctx)
		return nil, fmt.Errorf("mongo ping: %w", err)
	}
	return &Driver{Client: c, cfg: cfg}, nil
}

func (d *Driver) Close(ctx context.Context) error { return d.Client.Disconnect(ctx) }

// DataSizeBytes returns the logical document size (dbStats.dataSize) of
// the named database in bytes, or 0 on error. Used by the MCP
// snapshot-inspect size estimate.
func (d *Driver) DataSizeBytes(ctx context.Context, name string) (int64, error) {
	var res struct {
		DataSize float64 `bson:"dataSize"`
	}
	if err := d.Client.Database(name).RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&res); err != nil {
		return 0, err
	}
	return int64(res.DataSize), nil
}

// DropMatching drops every database whose name starts with prefix.
// Returns the names that were actually dropped.
//
// Drops run in parallel (limit 6) — each Database.Drop is an
// independent admin command, so wall-clock for a paratest fan-out
// (1 source + N clones) drops linearly with concurrency.
func (d *Driver) DropMatching(ctx context.Context, prefix string) ([]string, error) {
	names, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	g, gctx := errgroup.WithContext(ctx)
	limit := 6
	if limit > len(names) && len(names) > 0 {
		limit = len(names)
	}
	if limit < 1 {
		limit = 1
	}
	g.SetLimit(limit)
	for _, n := range names {
		g.Go(func() error {
			if err := d.Client.Database(n).Drop(gctx); err != nil {
				return fmt.Errorf("drop mongo db %q: %w", n, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return names, nil
}

// ListMatching returns every database whose name starts with prefix.
func (d *Driver) ListMatching(ctx context.Context, prefix string) ([]string, error) {
	all, err := d.Client.ListDatabaseNames(ctx, struct{}{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range all {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out, nil
}
