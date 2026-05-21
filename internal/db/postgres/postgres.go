// Package postgres is the treeman PostgreSQL driver. Uses pgx via
// database/sql.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/jackc/pgx/v5/stdlib"

	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
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
func Connect(ctx context.Context, cfg config.PostgresConn) (*Driver, error) {
	if cfg.Port == 0 {
		cfg.Port = 5432
	}
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
	if err := reachability.Probe("postgres", cfg.Host, cfg.Port); err != nil {
		if cfg.Container != "" || cfg.ComposeService != "" {
			containerip.RefreshOpts(opts)
			if addr, e := containerip.ResolveAddr(opts); e == nil && addr != nil {
				cfg.Host = addr.Host
				if addr.Port != 0 {
					cfg.Port = addr.Port
				}
				if err2 := reachability.Probe("postgres", cfg.Host, cfg.Port); err2 != nil {
					return nil, err2
				}
			} else {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/postgres?sslmode=disable",
		url.QueryEscape(cfg.User),
		url.QueryEscape(cfg.Password),
		cfg.Host, cfg.Port,
	)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if cfg.PoolMax > 0 {
		db.SetMaxOpenConns(int(cfg.PoolMax))
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Driver{DB: db, cfg: cfg}, nil
}

func (d *Driver) Close() error { return d.DB.Close() }

func (d *Driver) EngineVersion(ctx context.Context) (string, error) {
	var v string
	if err := d.DB.QueryRowContext(ctx, "SELECT version()").Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

func (d *Driver) EnsureDB(ctx context.Context, name string) error {
	if err := validateIdent(name); err != nil {
		return err
	}
	exists, err := d.dbExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = d.execOutsideTx(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, name))
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
			// Boot any straggler connections so DROP can proceed.
			_, _ = d.DB.ExecContext(gctx,
				"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", n)
			if _, err := d.execOutsideTx(gctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, n)); err != nil {
				return fmt.Errorf("DROP DATABASE %q: %w", n, err)
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
	if err := validateIdent(source); err != nil {
		return err
	}
	if err := validateIdent(template); err != nil {
		return err
	}
	// Fence the source so no new connections sneak in mid-clone.
	if _, err := d.execOutsideTx(ctx, fmt.Sprintf(
		`ALTER DATABASE "%s" ALLOW_CONNECTIONS = false`, source)); err != nil {
		return err
	}
	defer func() {
		_, _ = d.execOutsideTx(ctx, fmt.Sprintf(
			`ALTER DATABASE "%s" ALLOW_CONNECTIONS = true`, source))
	}()
	_, _ = d.DB.ExecContext(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", source)
	_, _ = d.execOutsideTx(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, template))
	if _, err := d.execOutsideTx(ctx, fmt.Sprintf(
		`CREATE DATABASE "%s" TEMPLATE "%s"`, template, source)); err != nil {
		return err
	}
	return nil
}

func (d *Driver) SnapshotRestore(ctx context.Context, template, target string) error {
	if err := validateIdent(template); err != nil {
		return err
	}
	if err := validateIdent(target); err != nil {
		return err
	}
	if _, err := d.execOutsideTx(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, target)); err != nil {
		return err
	}
	_, err := d.execOutsideTx(ctx, fmt.Sprintf(
		`CREATE DATABASE "%s" TEMPLATE "%s"`, target, template))
	return err
}

func (d *Driver) DropSnapshot(ctx context.Context, template string) error {
	if err := validateIdent(template); err != nil {
		return err
	}
	_, err := d.execOutsideTx(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, template))
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

func validateIdent(s string) error {
	if s == "" {
		return fmt.Errorf("empty postgres identifier")
	}
	for _, c := range s {
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
		if !ok {
			return fmt.Errorf("invalid postgres identifier: %q", s)
		}
	}
	return nil
}
