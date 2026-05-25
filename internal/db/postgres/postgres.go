// Package postgres is the treeman PostgreSQL driver. Uses pgx via
// database/sql.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/ident"
	"github.com/stubbedev/treeman/internal/db/reachability"
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
	socketMode := strings.HasPrefix(cfg.Host, "/")
	if !socketMode {
		opts := containerip.Opts{
			Container:      cfg.Container,
			ComposeService: cfg.ComposeService,
			ComposeProject: cfg.ComposeProject,
			Engine:         cfg.ContainerEngine,
			Network:        cfg.Network,
			InternalPort:   cfg.Port,
		}
		if addr, err := containerip.ResolveAddr(opts); err != nil {
			return nil, fmt.Errorf("resolve container: %w", err)
		} else if addr != nil {
			cfg.Host = addr.Host
			if addr.Port != 0 {
				cfg.Port = addr.Port
			}
		}
		if err := reachability.ProbeCtx(ctx, "postgres", cfg.Host, cfg.Port); err != nil {
			if cfg.Container != "" || cfg.ComposeService != "" {
				containerip.RefreshOpts(opts)
				if addr, e := containerip.ResolveAddr(opts); e == nil && addr != nil {
					cfg.Host = addr.Host
					if addr.Port != 0 {
						cfg.Port = addr.Port
					}
					if err2 := reachability.ProbeCtx(ctx, "postgres", cfg.Host, cfg.Port); err2 != nil {
						return nil, err2
					}
				} else {
					return nil, err
				}
			} else {
				return nil, err
			}
		}
	}
	var dsn string
	if socketMode {
		dsn = fmt.Sprintf(
			"postgres://%s:%s@/postgres?sslmode=disable&host=%s",
			url.QueryEscape(cfg.User),
			url.QueryEscape(cfg.Password),
			url.QueryEscape(cfg.Host),
		)
	} else {
		dsn = fmt.Sprintf(
			"postgres://%s:%s@%s:%d/postgres?sslmode=disable",
			url.QueryEscape(cfg.User),
			url.QueryEscape(cfg.Password),
			cfg.Host, cfg.Port,
		)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	configurePool(db, int(cfg.PoolMax))
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Driver{DB: db, cfg: cfg}, nil
}

// configurePool — see mysql.configurePool for rationale; same
// defaults apply (database/sql's MaxIdleConns=2 default is
// pessimistic for pooled workloads, and unbounded conn lifetime
// trips errors after server restarts).
func configurePool(db *sql.DB, poolMax int) {
	if poolMax > 0 {
		db.SetMaxOpenConns(poolMax)
		idle := poolMax / 2
		if idle < 1 {
			idle = 1
		}
		db.SetMaxIdleConns(idle)
	}
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
}

func (d *Driver) Close() error { return d.DB.Close() }

// OpenScoped returns a fresh *sql.DB scoped to `dbName`. Use when
// you need to execute SQL that hits a specific database — Postgres
// has no `USE`, so each target DB needs its own connection. The
// returned DB owns its pool; the caller MUST Close it.
func (d *Driver) OpenScoped(ctx context.Context, dbName string) (*sql.DB, error) {
	var dsn string
	if strings.HasPrefix(d.cfg.Host, "/") {
		dsn = fmt.Sprintf(
			"postgres://%s:%s@/%s?sslmode=disable&host=%s",
			url.QueryEscape(d.cfg.User),
			url.QueryEscape(d.cfg.Password),
			url.QueryEscape(dbName),
			url.QueryEscape(d.cfg.Host),
		)
	} else {
		dsn = fmt.Sprintf(
			"postgres://%s:%s@%s:%d/%s?sslmode=disable",
			url.QueryEscape(d.cfg.User),
			url.QueryEscape(d.cfg.Password),
			d.cfg.Host, d.cfg.Port,
			url.QueryEscape(dbName),
		)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres %s: %w", dbName, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres %s: %w", dbName, err)
	}
	return db, nil
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
	_, err = d.execOutsideTx(ctx, "CREATE DATABASE "+qname)
	return err
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
		n := n
		g.Go(func() error {
			qn, err := ident.QuotePostgres(n)
			if err != nil {
				return err
			}
			// Boot any straggler connections so DROP can proceed.
			_, _ = d.DB.ExecContext(gctx,
				"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", n)
			if _, err := d.execOutsideTx(gctx, "DROP DATABASE IF EXISTS "+qn); err != nil {
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
	rows, err := d.DB.QueryContext(ctx,
		"SELECT datname FROM pg_database WHERE datname LIKE $1 ORDER BY datname",
		prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
func (d *Driver) SnapshotCreate(ctx context.Context, source, template string) error {
	qsource, err := ident.QuotePostgres(source)
	if err != nil {
		return err
	}
	qtemplate, err := ident.QuotePostgres(template)
	if err != nil {
		return err
	}
	// Fence the source so no new connections sneak in mid-clone.
	if _, err := d.execOutsideTx(ctx,
		"ALTER DATABASE "+qsource+" ALLOW_CONNECTIONS = false"); err != nil {
		return err
	}
	defer func() {
		// Use a fresh, short-lived context so the unfence runs even
		// when ctx has been cancelled or timed out mid-clone.
		// Otherwise a cancellation can leave the source DB
		// read-only-to-new-clients forever.
		bgctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = d.execOutsideTx(bgctx,
			"ALTER DATABASE "+qsource+" ALLOW_CONNECTIONS = true")
	}()
	_, _ = d.DB.ExecContext(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", source)
	_, _ = d.execOutsideTx(ctx, "DROP DATABASE IF EXISTS "+qtemplate)
	if _, err := d.execOutsideTx(ctx,
		"CREATE DATABASE "+qtemplate+" TEMPLATE "+qsource); err != nil {
		return err
	}
	return nil
}

func (d *Driver) SnapshotRestore(ctx context.Context, template, target string) error {
	qtemplate, err := ident.QuotePostgres(template)
	if err != nil {
		return err
	}
	qtarget, err := ident.QuotePostgres(target)
	if err != nil {
		return err
	}
	if _, err := d.execOutsideTx(ctx, "DROP DATABASE IF EXISTS "+qtarget); err != nil {
		return err
	}
	_, err = d.execOutsideTx(ctx,
		"CREATE DATABASE "+qtarget+" TEMPLATE "+qtemplate)
	return err
}

func (d *Driver) DropSnapshot(ctx context.Context, template string) error {
	qtemplate, err := ident.QuotePostgres(template)
	if err != nil {
		return err
	}
	_, err = d.execOutsideTx(ctx, "DROP DATABASE IF EXISTS "+qtemplate)
	return err
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
func (d *Driver) execOutsideTx(ctx context.Context, stmt string) (sql.Result, error) {
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ExecContext(ctx, stmt)
}

