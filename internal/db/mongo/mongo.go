// Package mongo is the treeman MongoDB driver. Uses
// go.mongodb.org/mongo-driver v2.
package mongo

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Driver wraps the mongo Client.
type Driver struct {
	Client *mongo.Client
}

// Connect parses cfg.URI, probes TCP reachability (when the URI
// isn't mongodb+srv:// where the host is a SRV record), and dials.
func Connect(ctx context.Context, cfg config.MongoConn) (*Driver, error) {
	uri := cfg.URI
	if cfg.Container != "" {
		ip, err := containerip.Resolve(cfg.Container, cfg.ContainerEngine)
		if err != nil {
			return nil, fmt.Errorf("resolve container %q: %w", cfg.Container, err)
		}
		if ip != "" {
			uri = containerip.RewriteHostPortInURI(uri, ip)
		}
	}
	if !strings.HasPrefix(uri, "mongodb+srv://") {
		if err := reachability.ProbeURL("mongodb", uri); err != nil {
			return nil, err
		}
	}
	c, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	if err := c.Ping(ctx, nil); err != nil {
		_ = c.Disconnect(ctx)
		return nil, fmt.Errorf("mongo ping: %w", err)
	}
	return &Driver{Client: c}, nil
}

func (d *Driver) Close(ctx context.Context) error { return d.Client.Disconnect(ctx) }

// DropMatching drops every database whose name starts with prefix.
// Returns the names that were actually dropped.
func (d *Driver) DropMatching(ctx context.Context, prefix string) ([]string, error) {
	names, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if err := d.Client.Database(n).Drop(ctx); err != nil {
			return nil, fmt.Errorf("drop mongo db %q: %w", n, err)
		}
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
