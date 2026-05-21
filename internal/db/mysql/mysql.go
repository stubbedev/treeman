// Package mysql is the treeman MySQL driver. database/sql +
// go-sql-driver/mysql uses MySQL's text protocol by default so DDL
// like USE / SHOW CREATE TABLE / SET SESSION just works — avoiding
// the 1295 "not supported in prepared statement protocol" error
// these statements trigger when issued via the prepared-statement
// protocol.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/sync/errgroup"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

// Driver wraps a *sql.DB plus the connection config used to open it.
type Driver struct {
	DB  *sql.DB
	cfg config.MysqlConn
}

// Connect opens a pooled mysql connection at the server level (no
// default DB scope so DDL like CREATE/DROP DATABASE works). TCP
// probe runs first so unreachable services surface a clean error
// before sqlx-style "connect refused" noise.
func Connect(ctx context.Context, cfg config.MysqlConn) (*Driver, error) {
	// Resolve `container:` to a bridge-network IP if configured.
	// Empty container falls through and uses cfg.Host directly.
	if ip, err := containerip.Resolve(cfg.Container, cfg.ContainerEngine); err != nil {
		return nil, fmt.Errorf("resolve container %q: %w", cfg.Container, err)
	} else if ip != "" {
		cfg.Host = ip
		if cfg.Port == 0 {
			cfg.Port = 3306
		}
	}
	if err := reachability.Probe("mysql", cfg.Host, cfg.Port); err != nil {
		// Container IP may have changed (restart); evict + retry once.
		if cfg.Container != "" {
			containerip.Refresh(cfg.Container, cfg.ContainerEngine)
			if ip, e := containerip.Resolve(cfg.Container, cfg.ContainerEngine); e == nil && ip != "" {
				cfg.Host = ip
				if err2 := reachability.Probe("mysql", cfg.Host, cfg.Port); err2 != nil {
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
		"%s:%s@tcp(%s:%d)/?charset=utf8mb4,utf8&parseTime=true&multiStatements=true",
		url.QueryEscape(cfg.User),
		url.QueryEscape(cfg.Password),
		cfg.Host, cfg.Port,
	)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if cfg.PoolMax > 0 {
		db.SetMaxOpenConns(int(cfg.PoolMax))
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &Driver{DB: db, cfg: cfg}, nil
}

// Close shuts down the pool.
func (d *Driver) Close() error { return d.DB.Close() }

// EngineVersion → `SELECT VERSION()`.
func (d *Driver) EngineVersion(ctx context.Context) (string, error) {
	var v string
	row := d.DB.QueryRowContext(ctx, "SELECT VERSION()")
	if err := row.Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// EnsureDB idempotently creates `name` with utf8mb4 / unicode_ci.
func (d *Driver) EnsureDB(ctx context.Context, name string) error {
	if err := validateIdent(name); err != nil {
		return err
	}
	stmt := fmt.Sprintf(
		"CREATE DATABASE IF NOT EXISTS `%s` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
		name,
	)
	if _, err := d.DB.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("CREATE DATABASE `%s`: %w", name, err)
	}
	return nil
}

// DropMatching drops every database whose name starts with prefix.
// Returns the names that were actually dropped.
//
// DROPs run in parallel (limit 6) because each `wt delete` typically
// fans out across source + template + N paratest clones (1+1+8 = 10
// databases on a default setup), and DROP DATABASE serializes only
// on the pg_database-equivalent lock — independent names don't
// contend.
func (d *Driver) DropMatching(ctx context.Context, prefix string) ([]string, error) {
	matched, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	g, gctx := errgroup.WithContext(ctx)
	limit := 6
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
			if err := validateIdent(n); err != nil {
				return err
			}
			if _, err := d.DB.ExecContext(gctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", n)); err != nil {
				return fmt.Errorf("DROP DATABASE `%s`: %w", n, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return matched, nil
}

// ListMatching returns every database whose name starts with prefix.
// CONVERT … USING utf8mb4 guards against the MySQL 8 VARBINARY-on-
// charset-mismatch decode failure that information_schema otherwise
// returns when the server and client default charsets disagree.
func (d *Driver) ListMatching(ctx context.Context, prefix string) ([]string, error) {
	like := strings.ReplaceAll(prefix, "_", `\_`) + "%"
	rows, err := d.DB.QueryContext(ctx,
		`SELECT CONVERT(SCHEMA_NAME USING utf8mb4) AS SCHEMA_NAME
		 FROM information_schema.SCHEMATA
		 WHERE SCHEMA_NAME LIKE ?`, like)
	if err != nil {
		return nil, fmt.Errorf("list mysql databases: %w", err)
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

// DatabaseExists returns true iff `name` is present in
// information_schema.SCHEMATA. Cheaper than ListMatching for an
// exact-name lookup.
func (d *Driver) DatabaseExists(ctx context.Context, name string) (bool, error) {
	row := d.DB.QueryRowContext(ctx,
		`SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?`, name)
	var one int
	err := row.Scan(&one)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FlushDatabase drops + recreates `name`, leaving it empty.
func (d *Driver) FlushDatabase(ctx context.Context, name string) error {
	if _, err := d.DropMatching(ctx, name); err != nil {
		return err
	}
	return d.EnsureDB(ctx, name)
}

// SnapshotCreate clones `source` into `template` table-by-table via
// `SHOW CREATE TABLE` + `INSERT INTO target.t SELECT * FROM source.t`.
// `USE` and `SHOW CREATE TABLE` go through MySQL's text protocol by
// default in database/sql so neither hits the 1295 prepared-protocol
// error.
//
// Tables clone in parallel (errgroup, limited to mysqlCloneFanout
// connections), views replay serially afterwards because views can
// reference other views / tables and engines reject CREATE VIEW
// against a missing dependency. Each parallel connection re-applies
// the session flags (foreign_key_checks=0, etc.) since SET SESSION
// is per-conn and the database/sql pool shuffles connections per
// statement otherwise.
func (d *Driver) SnapshotCreate(ctx context.Context, source, template string) error {
	if err := validateIdent(source); err != nil {
		return err
	}
	if err := validateIdent(template); err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", template)); err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx, fmt.Sprintf(
		"CREATE DATABASE `%s` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
		template)); err != nil {
		return err
	}

	rows, err := d.DB.QueryContext(ctx, `
		SELECT
			CONVERT(TABLE_NAME USING utf8mb4) AS TABLE_NAME,
			CONVERT(TABLE_TYPE USING utf8mb4) AS TABLE_TYPE
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME
	`, source)
	if err != nil {
		return err
	}
	defer rows.Close()
	type t struct{ name, kind string }
	var tables, views []t
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err != nil {
			return err
		}
		row := t{name: n, kind: k}
		if k == "VIEW" {
			views = append(views, row)
		} else {
			tables = append(tables, row)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Parallel table clone. errgroup limits concurrency; each worker
	// acquires its own pooled connection so session vars stick.
	g, gctx := errgroup.WithContext(ctx)
	limit := mysqlCloneFanout
	if limit > len(tables) && len(tables) > 0 {
		limit = len(tables)
	}
	if limit < 1 {
		limit = 1
	}
	g.SetLimit(limit)
	for _, tbl := range tables {
		tbl := tbl
		g.Go(func() error {
			return cloneOneTable(gctx, d.DB, source, template, tbl.name)
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Views need their dependencies in place — defer to a single
	// serial pass after the parallel table copy finishes.
	if len(views) > 0 {
		conn, err := d.DB.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		if err := applyCloneSession(ctx, conn); err != nil {
			return err
		}
		for _, v := range views {
			if err := validateIdent(v.name); err != nil {
				return err
			}
			row := conn.QueryRowContext(ctx, fmt.Sprintf("SHOW CREATE VIEW `%s`.`%s`", source, v.name))
			var name, createStmt string
			// SHOW CREATE VIEW returns four columns; scan into
			// dummies for the extras.
			var charset, collation string
			if err := row.Scan(&name, &createStmt, &charset, &collation); err != nil {
				return fmt.Errorf("SHOW CREATE VIEW `%s`.`%s`: %w", source, v.name, err)
			}
			if _, err := conn.ExecContext(ctx, fmt.Sprintf("USE `%s`", template)); err != nil {
				return fmt.Errorf("USE `%s`: %w", template, err)
			}
			if _, err := conn.ExecContext(ctx, createStmt); err != nil {
				return fmt.Errorf("recreate view `%s`: %w", v.name, err)
			}
		}
	}
	return nil
}

// mysqlCloneFanout caps how many table copies run in parallel inside
// SnapshotCreate. 6 is a pragmatic default: a connection per worker
// stays comfortably under the typical 151-conn MySQL default, and
// goes wide enough to hide round-trip latency on schemas with many
// small tables.
const mysqlCloneFanout = 6

// applyCloneSession enables the session flags needed to skip
// constraint enforcement and binlog overhead during the bulk copy.
// `sql_log_bin` only takes effect when the connecting user has
// SUPER, so errors are tolerated.
func applyCloneSession(ctx context.Context, conn *sql.Conn) error {
	for _, s := range []string{
		"SET SESSION foreign_key_checks=0",
		"SET SESSION unique_checks=0",
		"SET SESSION sql_log_bin=0",
	} {
		_, _ = conn.ExecContext(ctx, s)
	}
	return nil
}

// cloneOneTable replicates a single base table from source into
// template. SHOW CREATE TABLE preserves indexes, constraints, and
// AUTO_INCREMENT. The INSERT … SELECT projection enumerates only
// non-generated columns (MySQL error 3105 otherwise).
func cloneOneTable(ctx context.Context, db *sql.DB, source, template, name string) error {
	if err := validateIdent(name); err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := applyCloneSession(ctx, conn); err != nil {
		return err
	}
	row := conn.QueryRowContext(ctx, fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", source, name))
	var gotName, createStmt string
	if err := row.Scan(&gotName, &createStmt); err != nil {
		return fmt.Errorf("SHOW CREATE TABLE `%s`.`%s`: %w", source, name, err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("USE `%s`", template)); err != nil {
		return fmt.Errorf("USE `%s`: %w", template, err)
	}
	if _, err := conn.ExecContext(ctx, createStmt); err != nil {
		return fmt.Errorf("recreate `%s`: %w", name, err)
	}
	cols, err := nonGeneratedColumns(ctx, conn, source, name)
	if err != nil {
		return fmt.Errorf("list columns for `%s`: %w", name, err)
	}
	if len(cols) == 0 {
		// Table is composed entirely of generated columns. The
		// schema clone already populates them on insert; skip the
		// data copy.
		return nil
	}
	colList := backtickJoin(cols)
	copyStmt := fmt.Sprintf(
		"INSERT INTO `%s`.`%s` (%s) SELECT %s FROM `%s`.`%s`",
		template, name, colList, colList, source, name)
	if _, err := conn.ExecContext(ctx, copyStmt); err != nil {
		return fmt.Errorf("copy data for `%s`: %w", name, err)
	}
	return nil
}

// nonGeneratedColumns lists the columns of `<schema>.<table>` whose
// EXTRA does not mark them as generated (STORED or VIRTUAL). Returned
// in ordinal-position order so the INSERT column list matches the
// SELECT projection one-to-one.
func nonGeneratedColumns(ctx context.Context, conn *sql.Conn, schema, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT CONVERT(COLUMN_NAME USING utf8mb4)
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		  AND EXTRA NOT LIKE '%GENERATED%'
		ORDER BY ORDINAL_POSITION`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func backtickJoin(cols []string) string {
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('`')
		b.WriteString(c)
		b.WriteByte('`')
	}
	return b.String()
}

// SnapshotRestore re-creates `target` from `template`. Symmetric to
// SnapshotCreate.
func (d *Driver) SnapshotRestore(ctx context.Context, template, target string) error {
	if err := validateIdent(template); err != nil {
		return err
	}
	if err := validateIdent(target); err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", target)); err != nil {
		return err
	}
	return d.SnapshotCreate(ctx, template, target)
}

// DropSnapshot drops a template DB. Idempotent.
func (d *Driver) DropSnapshot(ctx context.Context, template string) error {
	if err := validateIdent(template); err != nil {
		return err
	}
	_, err := d.DB.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", template))
	return err
}

// validateIdent rejects anything that isn't `[A-Za-z0-9_]+` so we can
// safely interpolate into backtick-quoted identifiers.
func validateIdent(s string) error {
	if s == "" {
		return fmt.Errorf("empty mysql identifier")
	}
	for _, c := range s {
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
		if !ok {
			return fmt.Errorf("invalid mysql identifier: %q", s)
		}
	}
	return nil
}
