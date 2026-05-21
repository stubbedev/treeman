// Package mysql is the treeman MySQL driver. Ported from
// `crates/treeman-db/src/mysql.rs`. database/sql + go-sql-driver/mysql
// uses MySQL's text protocol by default so DDL like USE / SHOW CREATE
// TABLE / SET SESSION just works — fixing the 1295 "not supported in
// prepared statement protocol" bug the Rust sqlx path needed
// raw_sql() to work around.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

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
func (d *Driver) DropMatching(ctx context.Context, prefix string) ([]string, error) {
	matched, err := d.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for _, n := range matched {
		if err := validateIdent(n); err != nil {
			return nil, err
		}
		if _, err := d.DB.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", n)); err != nil {
			return nil, fmt.Errorf("DROP DATABASE `%s`: %w", n, err)
		}
	}
	return matched, nil
}

// ListMatching returns every database whose name starts with prefix.
// CONVERT … USING utf8mb4 guards against the MySQL 8 VARBINARY-on-
// charset-mismatch decode failure the Rust sqlx path needed v0.3.1
// to patch.
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
// Ported from the Rust impl; the relevant detail is that `USE` and
// `SHOW CREATE TABLE` go through MySQL's text protocol by default in
// database/sql, fixing the v0.3.0 1295 bug for free.
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
	var tables []t
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err != nil {
			return err
		}
		tables = append(tables, t{name: n, kind: k})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, s := range []string{
		"SET SESSION foreign_key_checks=0",
		"SET SESSION unique_checks=0",
		"SET SESSION sql_log_bin=0",
	} {
		_, _ = conn.ExecContext(ctx, s) // best-effort; sql_log_bin requires SUPER
	}

	for _, tbl := range tables {
		if err := validateIdent(tbl.name); err != nil {
			return err
		}
		kind := "TABLE"
		if tbl.kind == "VIEW" {
			kind = "VIEW"
		}
		row := conn.QueryRowContext(ctx, fmt.Sprintf("SHOW CREATE %s `%s`.`%s`", kind, source, tbl.name))
		var name, createStmt string
		if err := row.Scan(&name, &createStmt); err != nil {
			return fmt.Errorf("SHOW CREATE %s `%s`.`%s`: %w", kind, source, tbl.name, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("USE `%s`", template)); err != nil {
			return fmt.Errorf("USE `%s`: %w", template, err)
		}
		if _, err := conn.ExecContext(ctx, createStmt); err != nil {
			return fmt.Errorf("recreate `%s`: %w", tbl.name, err)
		}
		if tbl.kind != "VIEW" {
			// Generated columns (STORED or VIRTUAL) reject any
			// caller-supplied value — `INSERT … SELECT *` trips
			// MySQL error 3105 ("The value specified for generated
			// column X is not allowed"). Enumerate the non-generated
			// columns explicitly so the column list matches on both
			// sides of the copy.
			cols, err := nonGeneratedColumns(ctx, conn, source, tbl.name)
			if err != nil {
				return fmt.Errorf("list columns for `%s`: %w", tbl.name, err)
			}
			if len(cols) == 0 {
				// Table is composed entirely of generated columns
				// (rare; the schema clone via SHOW CREATE TABLE
				// already populates them). Skip the data copy.
				continue
			}
			colList := backtickJoin(cols)
			copy := fmt.Sprintf(
				"INSERT INTO `%s`.`%s` (%s) SELECT %s FROM `%s`.`%s`",
				template, tbl.name, colList, colList, source, tbl.name)
			if _, err := conn.ExecContext(ctx, copy); err != nil {
				return fmt.Errorf("copy data for `%s`: %w", tbl.name, err)
			}
		}
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
