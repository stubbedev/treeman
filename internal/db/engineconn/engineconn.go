// Package engineconn is a uniform, engine-agnostic view over a connected
// database driver. The same "is the connection configured? connect, defer
// close, probe/drop" preamble was previously copy-pasted as a five-arm
// engine switch in internal/mcp (probe/drop tools), internal/snapshot
// (template GC) and internal/prepare (worktree teardown). Routing those
// sites through Conn + Connect keeps the per-engine connect/close/method
// mapping in one place.
//
// It deliberately exposes only the operations those dispatch sites share
// — version, existence, prefix drop, snapshot drop, size. Engine-specific
// work (dump load, snapshot create/restore, connection caps, scoped
// handles) stays on the concrete drivers, reached directly by the
// cold-build paths that need it.
package engineconn

import (
	"context"

	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	"github.com/stubbedev/treeman/internal/engine"
)

// Conn is the uniform view over a connected engine driver.
type Conn interface {
	// Close releases the driver's handle. Safe to call once; the caller
	// owns the lifecycle (typically `defer conn.Close()`).
	Close() error
	// EngineVersion returns the server version string, or "" if unknown.
	EngineVersion(ctx context.Context) (string, error)
	// Exists reports whether the named target is live — a database name
	// for name-scoped engines, a key/index prefix for prefix-scoped ones.
	Exists(ctx context.Context, name string) (bool, error)
	// DropMatching prefix-reaps every namespace under `name` and returns
	// the count dropped. For name-scoped engines this is the worktree DB
	// family (name + per-test clones); for prefix-scoped engines it's the
	// key/index prefix.
	DropMatching(ctx context.Context, name string) (int, error)
	// DropSnapshot drops exactly the named cached template (not a prefix
	// reap). Idempotent.
	DropSnapshot(ctx context.Context, name string) error
	// ListMatching returns every namespace whose name starts with
	// `prefix` — database names for name-scoped engines, index names
	// for ES. Redis returns (nil, nil): enumerating a key prefix means
	// a full SCAN, too expensive for inspection probes.
	ListMatching(ctx context.Context, prefix string) ([]string, error)
	// SizeKB is the on-disk size of `name` in KiB, or 0 when the engine
	// exposes no size.
	SizeKB(ctx context.Context, name string) int64
}

type mysqlConn struct{ d *dbmysql.Driver }

func (c mysqlConn) Close() error                                      { return c.d.Close() }
func (c mysqlConn) EngineVersion(ctx context.Context) (string, error) { return c.d.EngineVersion(ctx) }
func (c mysqlConn) Exists(ctx context.Context, n string) (bool, error) {
	return c.d.DatabaseExists(ctx, n)
}

func (c mysqlConn) DropMatching(ctx context.Context, n string) (int, error) {
	dropped, err := c.d.DropMatching(ctx, n)
	return len(dropped), err
}
func (c mysqlConn) DropSnapshot(ctx context.Context, n string) error { return c.d.DropSnapshot(ctx, n) }

func (c mysqlConn) ListMatching(ctx context.Context, p string) ([]string, error) {
	return c.d.ListMatching(ctx, p)
}

func (c mysqlConn) SizeKB(ctx context.Context, n string) int64 {
	// SUM(data_length+index_length) — bytes-on-disk per
	// information_schema.tables.
	var size int64
	_ = c.d.DB.QueryRowContext(ctx, `
		SELECT IFNULL(SUM(data_length + index_length), 0)
		FROM information_schema.tables WHERE table_schema = ?
	`, n).Scan(&size)
	return size / 1024
}

type postgresConn struct{ d *dbpostgres.Driver }

func (c postgresConn) Close() error { return c.d.Close() }
func (c postgresConn) EngineVersion(ctx context.Context) (string, error) {
	return c.d.EngineVersion(ctx)
}

func (c postgresConn) Exists(ctx context.Context, n string) (bool, error) {
	return c.d.DatabaseExists(ctx, n)
}

func (c postgresConn) DropMatching(ctx context.Context, n string) (int, error) {
	dropped, err := c.d.DropMatching(ctx, n)
	return len(dropped), err
}

func (c postgresConn) DropSnapshot(ctx context.Context, n string) error {
	return c.d.DropSnapshot(ctx, n)
}

func (c postgresConn) ListMatching(ctx context.Context, p string) ([]string, error) {
	return c.d.ListMatching(ctx, p)
}

func (c postgresConn) SizeKB(ctx context.Context, n string) int64 {
	var size int64
	_ = c.d.DB.QueryRowContext(ctx, "SELECT pg_database_size($1)/1024", n).Scan(&size)
	return size
}

// mongoConn carries the connect-time ctx so Close can satisfy the
// ctx-less Conn.Close — the mongo driver's Close takes a context.
type mongoConn struct {
	d   *dbmongo.Driver
	ctx context.Context
}

func (c mongoConn) Close() error                                      { return c.d.Close(c.ctx) }
func (c mongoConn) EngineVersion(ctx context.Context) (string, error) { return c.d.EngineVersion(ctx) }
func (c mongoConn) Exists(ctx context.Context, n string) (bool, error) {
	return c.d.DatabaseExists(ctx, n)
}

func (c mongoConn) DropMatching(ctx context.Context, n string) (int, error) {
	dropped, err := c.d.DropMatching(ctx, n)
	return len(dropped), err
}
func (c mongoConn) DropSnapshot(ctx context.Context, n string) error { return c.d.DropSnapshot(ctx, n) }

func (c mongoConn) ListMatching(ctx context.Context, p string) ([]string, error) {
	return c.d.ListMatching(ctx, p)
}

func (c mongoConn) SizeKB(ctx context.Context, n string) int64 {
	b, _ := c.d.DataSizeBytes(ctx, n)
	return b / 1024
}

type redisConn struct{ d *dbredis.Driver }

func (c redisConn) Close() error                                      { return c.d.Close() }
func (c redisConn) EngineVersion(ctx context.Context) (string, error) { return c.d.EngineVersion(ctx) }
func (c redisConn) Exists(ctx context.Context, n string) (bool, error) {
	return c.d.PrefixExists(ctx, n)
}

func (c redisConn) DropMatching(ctx context.Context, n string) (int, error) {
	return c.d.DropPrefix(ctx, n)
}
func (c redisConn) DropSnapshot(ctx context.Context, n string) error { return c.d.DropSnapshot(ctx, n) }

// ListMatching is nil for Redis: enumerating a key prefix needs a full
// SCAN — too expensive for an inspection probe (mirrors SizeKB).
func (redisConn) ListMatching(context.Context, string) ([]string, error) { return nil, nil }

// SizeKB is 0 for Redis: prefix size has no cheap server-side query —
// it would need a SCAN + per-key MEMORY USAGE, too expensive for an
// inspection probe. Left unimplemented rather than stubbed misleadingly.
func (redisConn) SizeKB(context.Context, string) int64 { return 0 }

// esConn — the ES driver holds no closable handle, so Close is a no-op.
type esConn struct{ d *dbes.Driver }

func (esConn) Close() error                                        { return nil }
func (c esConn) EngineVersion(ctx context.Context) (string, error) { return c.d.EngineVersion(ctx) }
func (c esConn) Exists(ctx context.Context, n string) (bool, error) {
	matched, err := c.d.ListMatching(ctx, n)
	return len(matched) > 0, err
}

func (c esConn) DropMatching(ctx context.Context, n string) (int, error) {
	dropped, err := c.d.DropMatching(ctx, n)
	return len(dropped), err
}
func (c esConn) DropSnapshot(ctx context.Context, n string) error { return c.d.DropSnapshot(ctx, n) }

func (c esConn) ListMatching(ctx context.Context, p string) ([]string, error) {
	return c.d.ListMatching(ctx, p)
}

func (c esConn) SizeKB(ctx context.Context, n string) int64 {
	b, _ := c.d.StoreSizeBytes(ctx, n)
	return b / 1024
}

// Configured reports whether `fam` has a connection block in cfg —
// i.e. whether Connect would return configured=true. Lets callers cheaply
// skip an engine that was never wired up without opening a connection.
func Configured(cfg *config.Config, fam engine.Family) bool {
	switch fam {
	case engine.FamilyMySQL:
		return cfg.Connections.Mysql != nil
	case engine.FamilyPostgres:
		return cfg.Connections.Postgres != nil
	case engine.FamilyMongo:
		return cfg.Connections.Mongodb != nil
	case engine.FamilyRedis:
		return cfg.Connections.Redis != nil
	case engine.FamilyES:
		return cfg.Connections.Elasticsearch != nil
	}
	return false
}

// Connect dials the engine for `fam`, returning a uniform Conn.
// `configured` is false (with a nil Conn + nil error) when the engine has
// no connection block, letting callers distinguish "not wired up" from
// "configured but unreachable". The caller owns Close.
func Connect(ctx context.Context, cfg *config.Config, fam engine.Family) (conn Conn, configured bool, err error) {
	switch fam {
	case engine.FamilyMySQL:
		if cfg.Connections.Mysql == nil {
			return nil, false, nil
		}
		d, e := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
		if e != nil {
			return nil, true, e
		}
		return mysqlConn{d}, true, nil
	case engine.FamilyPostgres:
		if cfg.Connections.Postgres == nil {
			return nil, false, nil
		}
		d, e := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
		if e != nil {
			return nil, true, e
		}
		return postgresConn{d}, true, nil
	case engine.FamilyMongo:
		if cfg.Connections.Mongodb == nil {
			return nil, false, nil
		}
		d, e := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
		if e != nil {
			return nil, true, e
		}
		return mongoConn{d, ctx}, true, nil
	case engine.FamilyRedis:
		if cfg.Connections.Redis == nil {
			return nil, false, nil
		}
		d, e := dbredis.Connect(ctx, *cfg.Connections.Redis)
		if e != nil {
			return nil, true, e
		}
		return redisConn{d}, true, nil
	case engine.FamilyES:
		if cfg.Connections.Elasticsearch == nil {
			return nil, false, nil
		}
		d, e := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if e != nil {
			return nil, true, e
		}
		return esConn{d}, true, nil
	}
	return nil, false, nil
}
