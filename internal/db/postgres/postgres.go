// Package postgres is the treeman PostgreSQL driver. Uses pgx via
// database/sql.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	// Register the pgx database/sql driver under the name "pgx".
	_ "github.com/jackc/pgx/v5/stdlib"

	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/adapter"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/ident"
	"github.com/stubbedev/treeman/internal/db/reachability"
	"github.com/stubbedev/treeman/internal/migrations/framework"
)

// Driver wraps a server-level *sql.DB so DDL like CREATE DATABASE
// works.
type Driver struct {
	DB  *sql.DB
	cfg config.PostgresConn
}

// Connect opens a server-level pool against `postgres` DB so we can
// issue CREATE / DROP / TEMPLATE statements at the cluster level.
// Three connection modes are auto-detected from cfg:
//
//   - Container/ComposeService set → containerip rewrites Host:Port
//     to the published mapping (or bridge-network IP fallback), then
//     dials TCP.
//   - cfg.Host starts with "/" → unix-socket DSN
//     (`postgres:///postgres?host=/var/run/postgresql`), TCP probe
//     skipped.
//   - Otherwise → plain TCP, probe-first.
func Connect(ctx context.Context, cfg config.PostgresConn) (*Driver, error) {
	if cfg.Port == 0 {
		cfg.Port = 5432
	}
	if strings.HasPrefix(cfg.Host, "/") {
		return adapter.Await(ctx, func(ctx context.Context) (*Driver, error) { return dial(ctx, cfg, cfg.Host) })
	}
	return adapter.Connect(ctx, "postgres", containerip.FromRef(cfg.ContainerRef, cfg.Port), cfg.Host, cfg.Port,
		func(ctx context.Context, host string, port uint16) (*Driver, error) {
			c := cfg
			c.Host, c.Port = host, port
			return dial(ctx, c, net.JoinHostPort(host, strconv.Itoa(int(port))))
		})
}

// sqlstateCannotConnectNow is the SQLSTATE a postgres server answers a
// connection with while it starts up, shuts down or recovers.
const sqlstateCannotConnectNow = "57P03"

// dial opens a server-level pool against cfg and completes one
// handshake. A handshake the server never got to answer (the
// connection refused, reset or closed), or answered with "the database
// system is starting up", is an engine-unreachable failure at addr.
func dial(ctx context.Context, cfg config.PostgresConn, addr string) (*Driver, error) {
	db, err := sql.Open("pgx", dsn(cfg, "postgres"))
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	adapter.ConfigurePool(db, int(cfg.PoolMax))
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateCannotConnectNow {
			return nil, fmt.Errorf("ping postgres: %w", reachability.StartingError("postgres", addr, err))
		}
		return nil, fmt.Errorf("ping postgres: %w", reachability.HandshakeError(ctx, "postgres", addr, err))
	}
	return &Driver{DB: db, cfg: cfg}, nil
}

// Endpoint is the host and port the driver dials: the configured
// address after container resolution, or the unix socket path.
func (d *Driver) Endpoint() (host string, port uint16) { return d.cfg.Host, d.cfg.Port }

func (d *Driver) Close() error { return d.DB.Close() }

// dsn builds the pgx URL for dbName: a unix socket when cfg.Host is a
// directory path, TCP otherwise. url.URL escapes each part for its own
// position; QueryEscape, used before, writes a space as '+', which
// userinfo and path decoding keep literally, so a password or database
// name containing a space never matched.
func dsn(cfg config.PostgresConn, dbName string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(cfg.User, cfg.Password), Path: "/" + dbName}
	q := url.Values{"sslmode": {"disable"}}
	if strings.HasPrefix(cfg.Host, "/") {
		q.Set("host", cfg.Host)
	} else {
		u.Host = net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// OpenScoped returns a fresh *sql.DB scoped to `dbName`. Use when
// you need to execute SQL that hits a specific database — Postgres
// has no `USE`, so each target DB needs its own connection. The
// returned DB owns its pool; the caller MUST Close it.
func (d *Driver) OpenScoped(ctx context.Context, dbName string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn(d.cfg, dbName))
	if err != nil {
		return nil, fmt.Errorf("open postgres %s: %w", dbName, err)
	}
	// Apply the same pool tuning as the primary Connect pool so scoped
	// handles don't inherit the stdlib MaxIdleConns=2 / no-max-lifetime
	// defaults (idle-connection churn + stale conns across server recycle).
	adapter.ConfigurePool(db, int(d.cfg.PoolMax))
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres %s: %w", dbName, err)
	}
	return db, nil
}

// AppliedBatches reads t from database db (in its current schema): the
// batch of every migration it records, keyed by name. found is false
// when db has no such table with both columns.
func (d *Driver) AppliedBatches(ctx context.Context, db string, t framework.BatchTable) (map[string]int, bool, error) {
	qt, err := ident.QuotePostgres(t.Table)
	if err != nil {
		return nil, false, err
	}
	qn, err := ident.QuotePostgres(t.NameColumn)
	if err != nil {
		return nil, false, err
	}
	qb, err := ident.QuotePostgres(t.BatchColumn)
	if err != nil {
		return nil, false, err
	}
	scoped, err := d.OpenScoped(ctx, db)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = scoped.Close() }()
	batches, found, err := adapter.AppliedBatches(
		ctx,
		scoped,
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 AND column_name IN ($2, $3)`,
		[]any{t.Table, t.NameColumn, t.BatchColumn},
		"SELECT "+qn+", "+qb+" FROM "+qt,
	)
	if err != nil {
		return nil, false, fmt.Errorf("postgres %s.%s: %w", db, t.Table, err)
	}
	return batches, found, nil
}

func (d *Driver) EngineVersion(ctx context.Context) (string, error) {
	var v string
	if err := d.DB.QueryRowContext(ctx, "SELECT version()").Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// MaxConnections returns the server's max_connections GUC. SHOW
// returns text; parse it with strconv. Same role as the mysql
// driver's MaxConnections — the fanout auto-tuner reads it to size
// the outer goroutine pool against the available connection budget.
func (d *Driver) MaxConnections(ctx context.Context) (int, error) {
	var v string
	if err := d.DB.QueryRowContext(ctx, "SHOW max_connections").Scan(&v); err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse max_connections %q: %w", v, err)
	}
	return n, nil
}

// WriteWatermark returns a sound, monotonic, PER-DATABASE write-counter
// token: tup_inserted + tup_updated + tup_deleted from pg_stat_database
// for `db`. The statistics collector is on by default (track_counts) and
// these counters only increase, so an unchanged token between two calls
// proves `db` received no row writes in between. Per-database (unlike the
// server-wide MySQL token), so it isn't perturbed by writes to sibling
// databases. An error or a missing row returns "" so the caller declines
// to skip work.
func (d *Driver) WriteWatermark(ctx context.Context, db string) (string, error) {
	var n sql.NullInt64
	err := d.DB.QueryRowContext(ctx,
		`SELECT COALESCE(tup_inserted,0) + COALESCE(tup_updated,0) + COALESCE(tup_deleted,0)
		 FROM pg_stat_database WHERE datname = $1`, db).Scan(&n)
	if err != nil {
		return "", err
	}
	return "pg:" + strconv.FormatInt(n.Int64, 10), nil
}

func (d *Driver) EnsureDB(ctx context.Context, name string) error {
	qname, err := ident.QuotePostgres(name)
	if err != nil {
		return err
	}
	exists, err := d.dbExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return d.execOutsideTx(ctx, "CREATE DATABASE "+qname)
}

func (d *Driver) DropMatching(ctx context.Context, prefix string) ([]string, error) {
	matched, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	// Parallel DROPs: each one acquires a brief AccessExclusiveLock
	// on pg_database, so the engine serializes the lock-acquisition
	// step anyway — but the pg_terminate_backend call + WAL flush per
	// drop dominate latency on a paratest fan-out (~10 dbs). Limit
	// 4 keeps us well under the typical max_connections=100 while
	// extracting most of the wall-clock win.
	g, gctx := errgroup.WithContext(ctx)
	limit := 4
	if limit > len(matched) && len(matched) > 0 {
		limit = len(matched)
	}
	if limit < 1 {
		limit = 1
	}
	g.SetLimit(limit)
	for _, n := range matched {
		g.Go(func() error {
			qn, err := ident.QuotePostgres(n)
			if err != nil {
				return err
			}
			if err := d.terminateBackends(gctx, n); err != nil {
				return err
			}
			if err := d.execOutsideTx(gctx, "DROP DATABASE IF EXISTS "+qn); err != nil {
				return fmt.Errorf("DROP DATABASE %s: %w", qn, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return matched, nil
}

func (d *Driver) ListMatching(ctx context.Context, prefix string) ([]string, error) {
	// Escape LIKE metacharacters in the prefix so a literal `_` (present in
	// every templated `name_<slug>`) is not treated as a single-char
	// wildcard — otherwise DropMatching/FlushDatabase could reap sibling
	// databases. Postgres LIKE uses backslash as the default escape char,
	// matching the MySQL driver's identical guard.
	like := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(prefix) + "%"
	rows, err := d.DB.QueryContext(ctx,
		"SELECT datname FROM pg_database WHERE datname LIKE $1 ORDER BY datname",
		like)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *Driver) FlushDatabase(ctx context.Context, name string) error {
	if _, err := d.DropMatching(ctx, name); err != nil {
		return err
	}
	return d.EnsureDB(ctx, name)
}

// SnapshotCreate uses native `CREATE DATABASE … TEMPLATE` — the
// fastest path Postgres offers for cloning a database.
func (d *Driver) SnapshotCreate(ctx context.Context, source, template string) (err error) {
	qsource, err := ident.QuotePostgres(source)
	if err != nil {
		return err
	}
	qtemplate, err := ident.QuotePostgres(template)
	if err != nil {
		return err
	}
	// Fence the source so no new connections sneak in mid-clone.
	if err := d.execOutsideTx(ctx,
		"ALTER DATABASE "+qsource+" ALLOW_CONNECTIONS = false"); err != nil {
		return err
	}
	defer func() {
		bgctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if uerr := d.execOutsideTx(bgctx, "ALTER DATABASE "+qsource+" ALLOW_CONNECTIONS = true"); uerr != nil {
			err = errors.Join(err, fmt.Errorf("re-allow connections to %s after cloning it: %w", source, uerr))
		}
	}()
	if err := d.terminateBackends(ctx, source); err != nil {
		return err
	}
	if err := d.execOutsideTx(ctx, "DROP DATABASE IF EXISTS "+qtemplate); err != nil {
		return fmt.Errorf("drop the previous template %s: %w", template, err)
	}
	if err := d.execOutsideTx(ctx,
		"CREATE DATABASE "+qtemplate+" TEMPLATE "+qsource); err != nil {
		return err
	}
	return nil
}

// SnapshotRestore replaces `target` with a clone of `template`. The
// clone is made under a private name with `CREATE DATABASE … TEMPLATE`
// and swapped in by replaceDatabase, so `target` exists throughout.
// Both private databases are dropped on every return path; a crashed
// run's leftovers are reaped by a later restore once they are
// restorestage.MaxAge old.
func (d *Driver) SnapshotRestore(ctx context.Context, template, target string) (err error) {
	qtemplate, err := ident.QuotePostgres(template)
	if err != nil {
		return err
	}
	if err := ident.ValidatePostgres(target); err != nil {
		return err
	}
	stage := d.beginRestoreStage(ctx)
	defer d.dropRestoreStage(ctx, &err, template, target, stage.Incoming, stage.Retired)
	qincoming, err := ident.QuotePostgres(stage.Incoming)
	if err != nil {
		return err
	}
	if err := d.execOutsideTx(ctx, "CREATE DATABASE "+qincoming+" TEMPLATE "+qtemplate); err != nil {
		return fmt.Errorf("restore %s into %s: clone into %s: %w", template, target, stage.Incoming, err)
	}
	if err := d.replaceDatabase(ctx, stage.Incoming, target, stage.Retired); err != nil {
		return fmt.Errorf("restore %s into %s: %w", template, target, err)
	}
	return nil
}

// ReplaceDatabase renames `from` to `to`, replacing any existing `to`
// in the same transaction, so `to` never stops existing. Renaming is a
// catalog-only operation, constant-time regardless of size. Postgres
// requires zero live connections on both databases (true for idle
// pre-warmed spares), and fails when `from` is gone, so of two racing
// claimers of one spare exactly one wins.
func (d *Driver) ReplaceDatabase(ctx context.Context, from, to string) (err error) {
	if err := ident.ValidatePostgres(from); err != nil {
		return err
	}
	if err := ident.ValidatePostgres(to); err != nil {
		return err
	}
	stage := d.beginRestoreStage(ctx)
	defer d.dropRestoreStage(ctx, &err, from, to, stage.Retired)
	if err := d.replaceDatabase(ctx, from, to, stage.Retired); err != nil {
		return fmt.Errorf("replace %s with %s: %w", to, from, err)
	}
	return nil
}

func (d *Driver) DropSnapshot(ctx context.Context, template string) error {
	qtemplate, err := ident.QuotePostgres(template)
	if err != nil {
		return err
	}
	return d.execOutsideTx(ctx, "DROP DATABASE IF EXISTS "+qtemplate)
}

// DropDatabase drops EXACTLY the named database — never a prefix match.
// Unlike DropMatching (`LIKE 'name%'`, used to reap a worktree's whole
// clone family), this touches only the single name given. branch_scoped
// teardown/reset/empty must use it: the active namespace there is often
// a bare app DB name that is a prefix of every linked worktree's
// `name_<slug>`, so a prefix drop would wipe sibling worktrees'
// databases. Idempotent.
func (d *Driver) DropDatabase(ctx context.Context, name string) error {
	qn, err := ident.QuotePostgres(name)
	if err != nil {
		return err
	}
	return d.execOutsideTx(ctx, "DROP DATABASE IF EXISTS "+qn)
}

// DatabaseExists returns true iff the named DB is present in
// pg_database. Mirrors the MySQL driver's same-named method so the
// snapshot cache hit path is engine-agnostic at the call-site.
func (d *Driver) DatabaseExists(ctx context.Context, name string) (bool, error) {
	return d.dbExists(ctx, name)
}

func (d *Driver) dbExists(ctx context.Context, name string) (bool, error) {
	var n int
	row := d.DB.QueryRowContext(ctx, "SELECT 1 FROM pg_database WHERE datname = $1", name)
	if err := row.Scan(&n); err == sql.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// execOutsideTx runs a DDL statement that cannot live inside a
// transaction (CREATE / DROP / ALTER DATABASE).
func (d *Driver) execOutsideTx(ctx context.Context, stmt string) error {
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(ctx, stmt)
	return err
}

func (d *Driver) terminateBackends(ctx context.Context, name string) error {
	if _, err := d.DB.ExecContext(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name); err != nil {
		return fmt.Errorf("terminate the connections to %s: %w", name, err)
	}
	return nil
}
