// Package postgres is the treeman PostgreSQL driver. Ported from
// `crates/treeman-db/src/postgres.rs`. Uses pgx via database/sql.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/jackc/pgx/v5/stdlib"

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
	if ip, err := containerip.Resolve(cfg.Container, cfg.ContainerEngine); err != nil {
		return nil, fmt.Errorf("resolve container %q: %w", cfg.Container, err)
	} else if ip != "" {
		cfg.Host = ip
		if cfg.Port == 0 {
			cfg.Port = 5432
		}
	}
	if err := reachability.Probe("postgres", cfg.Host, cfg.Port); err != nil {
		if cfg.Container != "" {
			containerip.Refresh(cfg.Container, cfg.ContainerEngine)
			if ip, e := containerip.Resolve(cfg.Container, cfg.ContainerEngine); e == nil && ip != "" {
				cfg.Host = ip
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
	for _, n := range matched {
		// Boot any straggler connections so DROP can proceed.
		_, _ = d.DB.ExecContext(ctx,
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", n)
		_, err := d.execOutsideTx(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, n))
		if err != nil {
			return nil, fmt.Errorf("DROP DATABASE %q: %w", n, err)
		}
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
