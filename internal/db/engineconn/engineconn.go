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
	"fmt"

	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	dbs3 "github.com/stubbedev/treeman/internal/db/s3"
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
	SizeKB(ctx context.Context, name string) (int64, error)
}

// SnapshotCreator is the optional capability of creating `target` as a
// copy of `template` through the engine's cheapest native path
// (Postgres CREATE DATABASE … TEMPLATE, MySQL direct physical clone).
// Backs spare-pool replenishment (#53). Drivers without it simply
// don't implement the interface; call sites type-assert.
type SnapshotCreator interface {
	CreateSnapshot(ctx context.Context, template, target string) error
}

// SpareClaimer is the optional capability of landing a pre-warmed
// spare onto `target` cheaply, replacing `target` in place so it never
// stops existing — Postgres by renaming the spare over it (a
// catalog-only, millisecond operation), MySQL by a staged physical
// clone of the spare swapped into it (file-copy import, no logical dump
// load) (#53, #186).
type SpareClaimer interface {
	ClaimSpare(ctx context.Context, spare, target string) error
}

// DatabaseDropper is the capability of dropping exactly one named
// database, never a prefix family. The name-scoped server engines
// (MySQL, Postgres, Mongo) implement it; worktree teardown drops the
// names it owns through it so a bare or short name can't reach a
// sibling worktree's databases.
type DatabaseDropper interface {
	DropDatabase(ctx context.Context, name string) error
}

// NewPostgresConn wraps an already-connected driver in the Conn view —
// for sites that hold the concrete driver and want the capability
// interfaces without re-dialing.
func NewPostgresConn(d *dbpostgres.Driver) Conn { return postgresConn{d} }

// NewMySQLConn is NewPostgresConn for MySQL.
func NewMySQLConn(d *dbmysql.Driver) Conn { return mysqlConn{d} }

func (c postgresConn) CreateSnapshot(ctx context.Context, template, target string) error {
	return c.d.SnapshotRestore(ctx, template, target)
}

func (c postgresConn) ClaimSpare(ctx context.Context, spare, target string) error {
	return c.d.ReplaceDatabase(ctx, spare, target)
}

func (c mysqlConn) CreateSnapshot(ctx context.Context, template, target string) error {
	return c.d.SnapshotRestore(ctx, template, target)
}

func (c mysqlConn) ClaimSpare(ctx context.Context, spare, target string) error {
	return c.d.SnapshotRestore(ctx, spare, target)
}

type mysqlConn struct{ d *dbmysql.Driver }

func (c mysqlConn) DropDatabase(ctx context.Context, n string) error { return c.d.DropDatabase(ctx, n) }

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

func (c mysqlConn) SizeKB(ctx context.Context, n string) (int64, error) {
	var size int64
	if err := c.d.DB.QueryRowContext(ctx, `
		SELECT IFNULL(SUM(data_length + index_length), 0)
		FROM information_schema.tables WHERE table_schema = ?
	`, n).Scan(&size); err != nil {
		return 0, fmt.Errorf("mysql: size of %s: %w", n, err)
	}
	return size / 1024, nil
}

type postgresConn struct{ d *dbpostgres.Driver }

func (c postgresConn) DropDatabase(ctx context.Context, n string) error {
	return c.d.DropDatabase(ctx, n)
}

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

func (c postgresConn) SizeKB(ctx context.Context, n string) (int64, error) {
	var size int64
	if err := c.d.DB.QueryRowContext(ctx, "SELECT pg_database_size($1)/1024", n).Scan(&size); err != nil {
		return 0, fmt.Errorf("postgres: size of %s: %w", n, err)
	}
	return size, nil
}

// mongoConn carries the connect-time ctx so Close can satisfy the
// ctx-less Conn.Close — the mongo driver's Close takes a context.
type mongoConn struct {
	d   *dbmongo.Driver
	ctx context.Context
}

func (c mongoConn) DropDatabase(ctx context.Context, n string) error { return c.d.DropDatabase(ctx, n) }

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

func (c mongoConn) SizeKB(ctx context.Context, n string) (int64, error) {
	b, err := c.d.DataSizeBytes(ctx, n)
	if err != nil {
		return 0, fmt.Errorf("mongodb: size of %s: %w", n, err)
	}
	return b / 1024, nil
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

// ListMatching enumerates snapshot template key-prefixes under `p`
// (e.g. `_tm`) as distinct `_tm:<hex16>:` prefixes via a bounded SCAN.
// This is what re-enabled Redis in the orphan scanner: the SCAN runs
// once per doctor/GC probe, not per worktree op, so the old
// "too expensive for an inspection probe" caveat no longer buys
// anything — and stranded template prefixes previously leaked until a
// manual FLUSHDB.
func (c redisConn) ListMatching(ctx context.Context, p string) ([]string, error) {
	return c.d.ListTemplatePrefixes(ctx, p)
}

// SizeKB is 0 for Redis: prefix size has no cheap server-side query —
// it would need a SCAN + per-key MEMORY USAGE, too expensive for an
// inspection probe. Left unimplemented rather than stubbed misleadingly.
func (redisConn) SizeKB(context.Context, string) (int64, error) { return 0, nil }

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

func (c esConn) SizeKB(ctx context.Context, n string) (int64, error) {
	b, err := c.d.StoreSizeBytes(ctx, n)
	if err != nil {
		return 0, fmt.Errorf("elasticsearch: size of %s: %w", n, err)
	}
	return b / 1024, nil
}

// s3Conn — bucket-scoped object store. The aws-sdk client holds no
// closable handle, so Close is a no-op (like esConn). S3 carries no
// snapshots (validate.go rejects branch_scoped/test_clones/dump), so
// DropSnapshot is a no-op and SizeKB is 0 (ListBuckets exposes no cheap
// per-bucket byte count). Exists/DropMatching/ListMatching map straight
// onto the bucket-lifecycle driver.
type s3Conn struct{ d *dbs3.Driver }

func (s3Conn) Close() error                                        { return nil }
func (c s3Conn) EngineVersion(ctx context.Context) (string, error) { return c.d.EngineVersion(ctx) }
func (c s3Conn) Exists(ctx context.Context, n string) (bool, error) {
	return c.d.BucketExists(ctx, n)
}

func (c s3Conn) DropMatching(ctx context.Context, n string) (int, error) {
	dropped, err := c.d.DropMatching(ctx, n)
	return len(dropped), err
}

// DropSnapshot is a no-op: S3 persists no template (no object-level
// snapshot model), so the GC sweep never hands us a real template name.
func (s3Conn) DropSnapshot(context.Context, string) error { return nil }

func (c s3Conn) ListMatching(ctx context.Context, p string) ([]string, error) {
	return c.d.ListMatching(ctx, p)
}

// SizeKB is 0 for S3: ListBuckets exposes no per-bucket byte count and a
// full object walk is too expensive for an inspection probe.
func (s3Conn) SizeKB(context.Context, string) (int64, error) { return 0, nil }

// Configured reports whether (fam, name) has a usable connection in
// cfg — i.e. whether Connect would return configured=true. Lets callers
// cheaply skip an engine that was never wired up without opening a
// connection. The registry's cheap check, no dialing (#47). An empty
// name selects the singular block; a non-empty name must match a named
// block (`connections.<family>.<name>`).
func Configured(cfg *config.Config, fam engine.Family, name string) bool {
	f, ok := Factory(fam)
	if !ok {
		return false
	}
	return f.Configured(cfg, name)
}

// Connect dials the engine for (fam, name) via its registered
// DriverFactory, returning a uniform Conn. `configured` is false (with
// a nil Conn + nil error) when the engine has no connection block,
// letting callers distinguish "not wired up" from "configured but
// unreachable". The caller owns Close. A family with no registered
// factory reports the same shape as an unconfigured one — alias tables
// (engine.Known) decide which families are legal upstream of here.
func Connect(ctx context.Context, cfg *config.Config, fam engine.Family, name string) (conn Conn, configured bool, err error) {
	f, ok := Factory(fam)
	if !ok {
		return nil, false, nil
	}
	return f.Connect(ctx, cfg, name)
}
