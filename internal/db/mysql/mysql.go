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
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	gosqlmysql "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/adapter"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/ident"
)

// mysqlErrTableExists is MySQL/MariaDB Error 1050 ("Table '%s' already
// exists"). Surfaces in two distinct scenarios during snapshot clone:
//   - The target schema isn't actually empty (a prior aborted restore
//     left tables behind that DROP DATABASE then DROP TABLE should
//     have cleaned up but didn't on all engine versions/configurations).
//   - An orphan InnoDB tablespace file (.ibd) exists in the schema
//     directory without a matching data-dictionary entry. The next
//     CREATE TABLE tries to write the same path and 1050s.
//
// Both are recoverable by issuing DROP TABLE IF EXISTS for the
// specific table — that force-frees the dictionary row + tablespace
// file — and retrying CREATE TABLE. See SnapshotCreate.
const mysqlErrTableExists = 1050

// isTableExistsErr returns true when err is a *mysql.MySQLError with
// Number == 1050.
func isTableExistsErr(err error) bool {
	var me *gosqlmysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrTableExists
}

// Driver wraps a *sql.DB plus the connection config used to open it.
type Driver struct {
	DB  *sql.DB
	cfg config.MysqlConn

	// strategyMu guards lastCloneStrategy. Callers query via
	// LastCloneStrategy after a SnapshotCreate to learn which path
	// (physical / logical) actually ran for observability.
	strategyMu        sync.Mutex
	lastCloneStrategy CloneStrategy

	// stagesMu guards stages. Keyed by template name, each entry holds
	// a sync.Once that exports the (immutable) template's InnoDB
	// tablespaces to a staging dir exactly once. SnapshotRestore then
	// imports from staging without re-taking FLUSH TABLES … FOR EXPORT
	// on the shared template, so a fan-out of N restores no longer
	// serializes on that per-template export lock. See physical_stage.go.
	stagesMu sync.Mutex
	stages   map[string]*templateStage
}

// LastCloneStrategy returns the CloneStrategy used by the most recent
// SnapshotCreate call on this driver. "" before any call. Used by the
// prepare layer to emit a snapshot_clone_strategy event so users can
// see whether the fast physical path or the logical fallback ran.
func (d *Driver) LastCloneStrategy() CloneStrategy {
	d.strategyMu.Lock()
	defer d.strategyMu.Unlock()
	return d.lastCloneStrategy
}

// setLastStrategy is the writer side of LastCloneStrategy. Called from
// SnapshotCreate's dispatcher under strategyMu.
func (d *Driver) setLastStrategy(s CloneStrategy) {
	d.strategyMu.Lock()
	d.lastCloneStrategy = s
	d.strategyMu.Unlock()
}

// Connect opens a pooled mysql connection at the server level (no
// default DB scope so DDL like CREATE/DROP DATABASE works). Three
// connection modes are auto-detected from cfg:
//
//   - Container/ComposeService set → containerip rewrites Host:Port
//     to the published mapping (or bridge-network IP fallback), then
//     dials TCP.
//   - cfg.Host starts with "/" → unix-socket DSN
//     (`unix(/var/run/mysqld/mysqld.sock)`), TCP probe skipped.
//   - Otherwise → plain TCP. Probe runs first so unreachable
//     services surface a clean error before sqlx-style noise.
func Connect(ctx context.Context, cfg config.MysqlConn) (*Driver, error) {
	if cfg.Port == 0 {
		cfg.Port = 3306
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
		if err := adapter.ResolveAndProbe(ctx, "mysql", opts, &cfg.Host, &cfg.Port); err != nil {
			return nil, err
		}
	}
	var dsn string
	if socketMode {
		dsn = fmt.Sprintf(
			"%s:%s@unix(%s)/?charset=utf8mb4,utf8&parseTime=true&multiStatements=true",
			url.QueryEscape(cfg.User),
			url.QueryEscape(cfg.Password),
			cfg.Host,
		)
	} else {
		dsn = fmt.Sprintf(
			"%s:%s@tcp(%s:%d)/?charset=utf8mb4,utf8&parseTime=true&multiStatements=true",
			url.QueryEscape(cfg.User),
			url.QueryEscape(cfg.Password),
			cfg.Host, cfg.Port,
		)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	adapter.ConfigurePool(db, int(cfg.PoolMax))
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
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

// MaxConnections returns the server's @@max_connections setting.
// Used by the fanout auto-tuner to compute a safe outer-concurrency
// cap from the connection budget. Returns 0 + error on probe failure
// so callers can fall back to a conservative hardcoded default.
func (d *Driver) MaxConnections(ctx context.Context) (int, error) {
	var v int
	if err := d.DB.QueryRowContext(ctx, "SELECT @@max_connections").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// EnsureDB idempotently creates `name` with utf8mb4 / unicode_ci.
func (d *Driver) EnsureDB(ctx context.Context, name string) error {
	qname, err := ident.QuoteMySQL(name)
	if err != nil {
		return err
	}
	//nolint:gosec // qname is validated/quoted via ident.QuoteMySQL; no user values concatenated
	stmt := "CREATE DATABASE IF NOT EXISTS " + qname +
		" DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
	if _, err := d.DB.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("CREATE DATABASE %s: %w", qname, err)
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
		g.Go(func() error {
			qn, err := ident.QuoteMySQL(n)
			if err != nil {
				return err
			}
			if _, err := d.DB.ExecContext(gctx, "DROP DATABASE IF EXISTS "+qn); err != nil {
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

// SnapshotCreate clones `source` into `template` using the fastest
// available strategy:
//
//  1. PHYSICAL: InnoDB transferable tablespaces. Requires
//     `connections.mysql.container` / `compose_service` to be set so
//     treeman can `<container-engine> exec cp` the .ibd / .cfg files
//     inside the container (preserving the mysql:mysql ownership the
//     server needs to IMPORT them). Several times faster than the
//     logical path on non-trivial schemas because it skips the
//     INSERT ... SELECT round-trips entirely. See physical_clone.go.
//
//  2. LOGICAL: the serial-DDL + parallel-INSERT-SELECT loader (see
//     logicalSnapshotCreate). Always available; picked when the
//     physical preconditions aren't met OR when physical was
//     attempted but failed (the warning is logged and the fall-back
//     takes over).
//
// The strategy chosen for this call is recorded on the Driver and
// retrievable via LastCloneStrategy(); the prepare layer reads it to
// emit a snapshot_clone_strategy event.
func (d *Driver) SnapshotCreate(ctx context.Context, source, template string) error {
	if ok, perr := d.tryPhysicalSnapshotCreate(ctx, source, template); ok {
		d.setLastStrategy(CloneStrategyPhysical)
		return nil
	} else if perr != nil {
		var skip *physicalSkippedError
		if errors.As(perr, &skip) {
			slog.Debug("mysql physical clone preconditions not met; using logical fallback",
				"source", source, "template", template, "reason", skip.reason)
		} else {
			slog.Warn("mysql physical clone failed; falling back to logical clone",
				"source", source, "template", template, "error", perr)
		}
	}
	d.setLastStrategy(CloneStrategyLogical)
	return d.logicalSnapshotCreate(ctx, source, template)
}

// logicalSnapshotCreate is the pre-physical implementation kept as the
// always-available fallback. Clones `source` into `template` in two
// phases:
//
//  1. Serial DDL pass on a single connection — every base table's
//     `CREATE TABLE` runs against the new template, and the
//     non-generated column list is captured for the data pass.
//     Doing DDL serially avoids the InnoDB data-dictionary
//     deadlocks (Error 1213) that parallel CREATE × INSERT pairs
//     used to trigger on schemas with many FK-linked tables.
//  2. Parallel data copy via errgroup (mysqlCloneFanout workers) —
//     each worker holds its own pooled connection so the
//     `SET SESSION` flags from applyCloneSession remain in scope,
//     and the INSERTs hit pre-created destination tables so the
//     lock graph stays purely data-side.
//
// Views replay serially afterwards because views can reference
// other views / tables and engines reject CREATE VIEW against a
// missing dependency.
func (d *Driver) logicalSnapshotCreate(ctx context.Context, source, template string) error {
	qsource, err := ident.QuoteMySQL(source)
	if err != nil {
		return err
	}
	qtemplate, err := ident.QuoteMySQL(template)
	if err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qtemplate); err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx,
		"CREATE DATABASE "+qtemplate+" DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		return err
	}

	tables, views, err := d.listTablesAndViews(ctx, source)
	if err != nil {
		return err
	}

	// Phase 1 — serial DDL pass on a single connection. Captures
	// each table's non-generated column list so Phase 2 can skip
	// the per-worker column lookup.
	jobs, err := d.cloneTablesDDL(ctx, source, qsource, qtemplate, tables)
	if err != nil {
		return err
	}

	// Phase 2 — parallel data copy against pre-created destination
	// tables.
	g, gctx := errgroup.WithContext(ctx)
	limit := mysqlCloneFanout
	if limit > len(jobs) && len(jobs) > 0 {
		limit = len(jobs)
	}
	if limit < 1 {
		limit = 1
	}
	g.SetLimit(limit)
	for _, j := range jobs {
		if len(j.cols) == 0 {
			// All columns generated; DDL pass already populated
			// the schema and there's nothing to copy.
			continue
		}

		g.Go(func() error {
			return copyOneTableData(gctx, d.DB, source, template, j.name, j.cols)
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Views need their dependencies in place — defer to a single
	// serial pass after the parallel table copy finishes.
	if len(views) > 0 {
		if err := d.cloneViews(ctx, qsource, qtemplate, views); err != nil {
			return err
		}
	}
	return nil
}

// schemaObject is one base table or view enumerated from
// information_schema for the clone. kind is the raw TABLE_TYPE.
type schemaObject struct{ name, kind string }

// tableJob carries a base table's name plus its non-generated column
// list (empty → all-generated, nothing to copy) from the serial DDL
// pass to the parallel data copy.
type tableJob struct {
	name string
	cols []string
}

// listTablesAndViews enumerates the base tables and views in `source`,
// splitting them into separate slices so SnapshotCreate can run the
// table DDL/data passes before replaying views (which may depend on
// tables or other views).
func (d *Driver) listTablesAndViews(ctx context.Context, source string) (tables, views []schemaObject, err error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT
			CONVERT(TABLE_NAME USING utf8mb4) AS TABLE_NAME,
			CONVERT(TABLE_TYPE USING utf8mb4) AS TABLE_TYPE
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME
	`, source)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n, k string
		if err := rows.Scan(&n, &k); err != nil {
			return nil, nil, err
		}
		row := schemaObject{name: n, kind: k}
		if k == "VIEW" {
			views = append(views, row)
		} else {
			tables = append(tables, row)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return tables, views, nil
}

// cloneTablesDDL runs SnapshotCreate's serial DDL pass on a single
// connection: it recreates every base table in the template and
// captures each table's non-generated column list for the parallel
// data copy. Returns the per-table jobs in source order.
func (d *Driver) cloneTablesDDL(ctx context.Context, source, qsource, qtemplate string, tables []schemaObject) ([]tableJob, error) {
	jobs := make([]tableJob, 0, len(tables))
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire DDL connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := applyCloneSession(ctx, conn); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "USE "+qtemplate); err != nil {
		return nil, fmt.Errorf("USE %s: %w", qtemplate, err)
	}
	// One schema-wide column lookup instead of a per-table query in
	// the loop: collapses N information_schema.COLUMNS round trips
	// into one on the cold-build critical path. Same gating (EXTRA
	// NOT LIKE '%GENERATED%') and ordinal ordering as the per-table
	// form, so each table's column list is byte-identical.
	colsByTable, err := nonGeneratedColumnsBySchema(ctx, conn, source)
	if err != nil {
		return nil, fmt.Errorf("list columns for %s: %w", qsource, err)
	}
	for _, tbl := range tables {
		if err := recreateTableDDL(ctx, conn, qsource, tbl.name); err != nil {
			return nil, err
		}
		jobs = append(jobs, tableJob{name: tbl.name, cols: colsByTable[tbl.name]})
	}
	return jobs, nil
}

// recreateTableDDL copies one table's schema from source into the
// connection's current (template) database via SHOW CREATE TABLE +
// CREATE TABLE, force-dropping and retrying on MySQL 1050.
func recreateTableDDL(ctx context.Context, conn *sql.Conn, qsource, name string) error {
	qtbl, err := ident.QuoteMySQL(name)
	if err != nil {
		return err
	}
	row := conn.QueryRowContext(ctx, "SHOW CREATE TABLE "+qsource+"."+qtbl)
	var gotName, createStmt string
	if err := row.Scan(&gotName, &createStmt); err != nil {
		return fmt.Errorf("SHOW CREATE TABLE %s.%s: %w", qsource, qtbl, err)
	}
	if _, err := conn.ExecContext(ctx, createStmt); err != nil {
		// MySQL 1050 = "Table already exists". Most often
		// caused by a prior aborted restore that left the
		// schema half-populated (or an orphan InnoDB .ibd
		// file post-crash). Force-drop the leftover and
		// retry so the fanout completes instead of bailing
		// the whole prepare. Conservative: only retry on
		// 1050; any other CREATE TABLE failure still aborts.
		if !isTableExistsErr(err) {
			return fmt.Errorf("recreate %s: %w", qtbl, err)
		}
		if _, dropErr := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+qtbl); dropErr != nil {
			return fmt.Errorf("recreate %s: drop stale table: %w (after %w)", qtbl, dropErr, err)
		}
		if _, retryErr := conn.ExecContext(ctx, createStmt); retryErr != nil {
			return fmt.Errorf("recreate %s after dropping stale table: %w", qtbl, retryErr)
		}
	}
	return nil
}

// cloneViews replays every view from source into the template on a
// single connection after the table copy finishes, so each CREATE VIEW
// sees its table / view dependencies already in place.
func (d *Driver) cloneViews(ctx context.Context, qsource, qtemplate string, views []schemaObject) error {
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := applyCloneSession(ctx, conn); err != nil {
		return err
	}
	for _, v := range views {
		qv, err := ident.QuoteMySQL(v.name)
		if err != nil {
			return err
		}
		row := conn.QueryRowContext(ctx, "SHOW CREATE VIEW "+qsource+"."+qv)
		var name, createStmt string
		// SHOW CREATE VIEW returns four columns; scan into
		// dummies for the extras.
		var charset, collation string
		if err := row.Scan(&name, &createStmt, &charset, &collation); err != nil {
			return fmt.Errorf("SHOW CREATE VIEW %s.%s: %w", qsource, qv, err)
		}
		if _, err := conn.ExecContext(ctx, "USE "+qtemplate); err != nil {
			return fmt.Errorf("USE %s: %w", qtemplate, err)
		}
		if _, err := conn.ExecContext(ctx, createStmt); err != nil {
			return fmt.Errorf("recreate view %s: %w", qv, err)
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
// `sql_log_bin=0` is load-bearing: without it MySQL falls back to
// locking source reads for INSERT … SELECT (binlog determinism
// rules), which is the root cause of the parallel-clone deadlocks
// SnapshotCreate used to hit. The grant requirement is
// SESSION_VARIABLES_ADMIN (or SUPER on 5.7); fail loudly when the
// user doesn't have it instead of silently regressing into lock
// contention.
func applyCloneSession(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "SET SESSION foreign_key_checks=0, unique_checks=0"); err != nil {
		return fmt.Errorf("disable foreign_key_checks / unique_checks: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_log_bin=0"); err != nil {
		return fmt.Errorf("disable sql_log_bin for snapshot clone (grant SESSION_VARIABLES_ADMIN or SUPER to the snapshot user): %w", err)
	}
	var v int
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.sql_log_bin").Scan(&v); err != nil {
		return fmt.Errorf("read back @@SESSION.sql_log_bin: %w", err)
	}
	if v != 0 {
		return fmt.Errorf(
			"SET SESSION sql_log_bin=0 was accepted but the session still reports %d; check user grants (need SESSION_VARIABLES_ADMIN or SUPER)",
			v,
		)
	}
	return nil
}

// copyOneTableData runs the data half of the clone for one table.
// The destination has already been created during SnapshotCreate's
// serial DDL pass, so this only issues the INSERT … SELECT against
// pre-existing tables — no concurrent CREATEs to contend with.
func copyOneTableData(ctx context.Context, db *sql.DB, source, template, name string, cols []string) error {
	qsource, err := ident.QuoteMySQL(source)
	if err != nil {
		return err
	}
	qtemplate, err := ident.QuoteMySQL(template)
	if err != nil {
		return err
	}
	qname, err := ident.QuoteMySQL(name)
	if err != nil {
		return err
	}
	colList, err := backtickJoin(cols)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := applyCloneSession(ctx, conn); err != nil {
		return err
	}
	//nolint:gosec // identifiers validated/quoted via ident.QuoteMySQL; no user values concatenated
	copyStmt := "INSERT INTO " + qtemplate + "." + qname + " (" + colList +
		") SELECT " + colList + " FROM " + qsource + "." + qname
	if _, err := conn.ExecContext(ctx, copyStmt); err != nil {
		return fmt.Errorf("copy data for %s: %w", qname, err)
	}
	return nil
}

// nonGeneratedColumnsBySchema lists, for every table in `schema`, the
// columns whose EXTRA does not mark them generated (STORED or
// VIRTUAL), keyed by table name. Each list is in ordinal-position
// order so the INSERT column list matches the SELECT projection
// one-to-one. One query for the whole schema replaces the former
// per-table lookup in SnapshotCreate's serial DDL pass.
func nonGeneratedColumnsBySchema(ctx context.Context, conn *sql.Conn, schema string) (map[string][]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT CONVERT(TABLE_NAME USING utf8mb4), CONVERT(COLUMN_NAME USING utf8mb4)
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ?
		  AND EXTRA NOT LIKE '%GENERATED%'
		ORDER BY TABLE_NAME, ORDINAL_POSITION`, schema)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanColumnsByTable(rows)
}

// scanColumnsByTable groups a (table_name, column_name) result set
// into a per-table column slice, preserving the row order so each
// table's list keeps the source's ordinal-position ordering (the
// query's ORDER BY guarantees that order). Split out from the query so
// the grouping is unit-testable without a live MySQL.
func scanColumnsByTable(rows *sql.Rows) (map[string][]string, error) {
	out := map[string][]string{}
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			return nil, err
		}
		out[tbl] = append(out[tbl], col)
	}
	return out, rows.Err()
}

func backtickJoin(cols []string) (string, error) {
	var b strings.Builder
	for i, c := range cols {
		if err := ident.ValidateMySQL(c); err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('`')
		b.WriteString(c)
		b.WriteByte('`')
	}
	return b.String(), nil
}

// SnapshotRestore re-creates `target` from `template`.
//
// Restore is the fan-out hot path: a single prepare clones one immutable
// template into N test databases concurrently. The physical clone in
// SnapshotCreate takes FLUSH TABLES <template>.* FOR EXPORT on the
// shared template and holds it across the .ibd copy, so N concurrent
// restores serialize on that one export lock — the dominant cost of a
// wide fan-out.
//
// To remove that contention the template's tablespaces are exported to
// a staging dir exactly once (ensureStage / stageTemplate, guarded by a
// per-template sync.Once); every restore then imports from staging via
// physicalRestoreFromStage, which copies plain files and takes no lock
// on the template. The fan-out is then bounded by disk + IMPORT, not by
// a serial export lock.
//
// Fallback chain is preserved exactly: when staging is unavailable
// (no ContainerRef, engine binary absent, empty secure_file_priv,
// non-InnoDB tables, …) or fails, the restore falls back to the always-
// available logical clone. Physical staging is never re-attempted via
// SnapshotCreate here — calling logicalSnapshotCreate directly avoids a
// pointless second FLUSH-FOR-EXPORT probe per clone.
func (d *Driver) SnapshotRestore(ctx context.Context, template, target string) error {
	qtarget, err := ident.QuoteMySQL(target)
	if err != nil {
		return err
	}
	if err := ident.ValidateMySQL(template); err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qtarget); err != nil {
		return err
	}

	st := d.ensureStage(ctx, template)
	switch {
	case st.skip != nil:
		slog.Debug("mysql physical staging unavailable; using logical restore",
			"template", template, "reason", st.skip.reason)
	case st.err != nil:
		slog.Warn("mysql physical staging failed; using logical restore",
			"template", template, "error", st.err)
	default:
		rerr := d.physicalRestoreFromStage(ctx, st, template, target)
		if rerr == nil {
			d.setLastStrategy(CloneStrategyPhysical)
			return nil
		}
		slog.Warn("mysql physical restore-from-stage failed; using logical restore",
			"template", template, "target", target, "error", rerr)
	}
	d.setLastStrategy(CloneStrategyLogical)
	return d.logicalSnapshotCreate(ctx, template, target)
}

// DropSnapshot drops a template DB. Idempotent. Also reaps any staging
// dir holding that template's exported tablespaces (best-effort — a
// fresh daemon that never staged this template still resolves the path
// from secure_file_priv, so eviction across process restarts is clean).
func (d *Driver) DropSnapshot(ctx context.Context, template string) error {
	qtemplate, err := ident.QuoteMySQL(template)
	if err != nil {
		return err
	}
	_, err = d.DB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qtemplate)
	d.dropStage(ctx, template)
	return err
}

// DropDatabase drops EXACTLY the named database — never a prefix match.
// Unlike DropMatching (which is `LIKE 'name%'` and used to reap a
// worktree's whole clone family), this touches only the single name
// given. branch_scoped teardown/reset/empty must use it: the active
// namespace there is often a bare app DB name (e.g. `kontainer`) that
// is a prefix of every linked worktree's `kontainer_<slug>`, so a
// prefix drop would wipe sibling worktrees' databases. Idempotent.
func (d *Driver) DropDatabase(ctx context.Context, name string) error {
	qn, err := ident.QuoteMySQL(name)
	if err != nil {
		return err
	}
	_, err = d.DB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qn)
	return err
}
