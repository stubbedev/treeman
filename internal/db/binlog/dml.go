package binlog

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/go-mysql-org/go-mysql/replication"

	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
)

// tableMeta caches the column list + primary-key column indices for
// a (schema, table) pair. We look these up once via information_
// schema and reuse across every event for the table until the
// process restarts or the structure changes (a DDL ALTER lands —
// the cached entry is invalidated on DDL apply).
type tableMeta struct {
	Columns []string // 1-to-1 with the binlog's row tuple, position-indexed
	PKCols  []int    // indices into Columns. Empty when table has no PK.
	// Writable lists the indices into Columns that the replayer is
	// allowed to set in INSERT / UPDATE. Generated columns (STORED
	// or VIRTUAL) are excluded — MySQL rejects any explicit value
	// for them with error 3105. When nil (legacy paths / tests that
	// don't populate it), every column is treated as writable.
	Writable []int
}

// writableIdx returns Writable, or a default 0..len(Columns)-1 slice
// when Writable hasn't been populated. Lets older callers keep
// working unchanged while the binlog replayer fills the field in.
func (t *tableMeta) writableIdx() []int {
	if t.Writable != nil {
		return t.Writable
	}
	out := make([]int, len(t.Columns))
	for i := range t.Columns {
		out[i] = i
	}
	return out
}

// metaCache is keyed by `<schema>.<table>`.
type metaCache struct {
	mu sync.Mutex
	m  map[string]*tableMeta
}

func newMetaCache() *metaCache { return &metaCache{m: map[string]*tableMeta{}} }

func (c *metaCache) get(key string) (*tableMeta, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[key]
	return t, ok
}

func (c *metaCache) put(key string, t *tableMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = t
}

// invalidate drops cached metadata for a schema (e.g. after a DDL
// run that may have altered any table in it).
func (c *metaCache) invalidate(schema string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := schema + "."
	for k := range c.m {
		if strings.HasPrefix(k, prefix) {
			delete(c.m, k)
		}
	}
}

// resolveTableMeta returns the column + PK metadata for
// `schema.table`, populating the cache on first lookup. Binlog
// FULL row metadata is preferred when available because it
// eliminates a per-event round trip; we fall back to
// information_schema when the upstream server doesn't emit FULL.
func resolveTableMeta(
	ctx context.Context,
	drv *dbmysql.Driver,
	cache *metaCache,
	schema, table string,
	tableMap *replication.TableMapEvent,
) (*tableMeta, error) {
	key := schema + "." + table
	if t, ok := cache.get(key); ok {
		return t, nil
	}
	// Try FULL row metadata first.
	if tableMap != nil && len(tableMap.ColumnName) > 0 {
		cols := make([]string, len(tableMap.ColumnName))
		for i, c := range tableMap.ColumnName {
			cols[i] = string(c)
		}
		pk := pkColumnsFromMeta(tableMap, cols)
		if len(pk) == 0 {
			// Server didn't emit PK info — populate from
			// information_schema since UPDATE/DELETE need it.
			pkNames, err := primaryKeyColumns(ctx, drv, schema, table)
			if err == nil {
				pk = indicesOf(cols, pkNames)
			}
		}
		// information_schema is the only reliable source for which
		// columns are GENERATED — the binlog row metadata does not
		// expose this. One extra round trip per table-first-touch,
		// cached for the life of the meta entry.
		genNames, _ := generatedColumns(ctx, drv, schema, table)
		t := &tableMeta{Columns: cols, PKCols: pk, Writable: writableMask(cols, genNames)}
		cache.put(key, t)
		return t, nil
	}
	// Fall back to information_schema.
	cols, err := columnNames(ctx, drv, schema, table)
	if err != nil {
		return nil, fmt.Errorf("column lookup %s.%s: %w", schema, table, err)
	}
	pkNames, _ := primaryKeyColumns(ctx, drv, schema, table)
	genNames, _ := generatedColumns(ctx, drv, schema, table)
	t := &tableMeta{
		Columns:  cols,
		PKCols:   indicesOf(cols, pkNames),
		Writable: writableMask(cols, genNames),
	}
	cache.put(key, t)
	return t, nil
}

// generatedColumns returns the names of GENERATED columns (STORED or
// VIRTUAL) in `<schema>.<table>`. MySQL surfaces them via the EXTRA
// column on information_schema.COLUMNS.
func generatedColumns(ctx context.Context, drv *dbmysql.Driver, schema, table string) ([]string, error) {
	rows, err := drv.DB.QueryContext(ctx, `
		SELECT CONVERT(COLUMN_NAME USING utf8mb4)
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		  AND EXTRA LIKE '%GENERATED%'
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

// writableMask returns the indices of `cols` that are NOT in
// `generated`. Order-preserving so the INSERT column list matches the
// row-value projection.
func writableMask(cols, generated []string) []int {
	if len(generated) == 0 {
		return nil // sentinel: caller treats nil as "all writable"
	}
	gen := make(map[string]struct{}, len(generated))
	for _, g := range generated {
		gen[g] = struct{}{}
	}
	out := make([]int, 0, len(cols))
	for i, c := range cols {
		if _, ok := gen[c]; ok {
			continue
		}
		out = append(out, i)
	}
	return out
}

// pkColumnsFromMeta extracts the primary-key column indices from a
// TableMapEvent when the server's `binlog_row_metadata=FULL`
// populates SimplePrimaryKey / PrimaryKeyWithPrefix.
func pkColumnsFromMeta(tm *replication.TableMapEvent, cols []string) []int {
	if len(tm.PrimaryKey) > 0 {
		out := make([]int, 0, len(tm.PrimaryKey))
		for _, idx := range tm.PrimaryKey {
			if int(idx) < len(cols) {
				out = append(out, int(idx))
			}
		}
		return out
	}
	return nil
}

func columnNames(ctx context.Context, drv *dbmysql.Driver, schema, table string) ([]string, error) {
	rows, err := drv.DB.QueryContext(ctx, `
		SELECT CONVERT(COLUMN_NAME USING utf8mb4) AS COLUMN_NAME
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
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

func primaryKeyColumns(ctx context.Context, drv *dbmysql.Driver, schema, table string) ([]string, error) {
	rows, err := drv.DB.QueryContext(ctx, `
		SELECT CONVERT(COLUMN_NAME USING utf8mb4) AS COLUMN_NAME
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY'
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

func indicesOf(cols, names []string) []int {
	out := make([]int, 0, len(names))
	for _, n := range names {
		for i, c := range cols {
			if c == n {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// quoteIdent backticks a MySQL identifier. Doubles any embedded
// backtick the way MySQL expects when an identifier contains one.
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// buildInsert constructs `INSERT INTO db.tbl (col1, col2…) VALUES
// (?, ?…)` for one row. Columns are quoted; values are bound via
// the placeholder list returned alongside. Generated columns
// (tracked in meta.Writable) are dropped from both the column list
// and the value list so MySQL doesn't reject the statement with
// error 3105 ("value specified for generated column is not allowed").
func buildInsert(target, table string, meta *tableMeta, row []any) (string, []any) {
	idx := meta.writableIdx()
	colList := make([]string, len(idx))
	placeholders := make([]string, len(idx))
	args := make([]any, len(idx))
	for j, i := range idx {
		colList[j] = quoteIdent(meta.Columns[i])
		placeholders[j] = "?"
		args[j] = row[i]
	}
	q := "INSERT INTO " + quoteIdent(target) + "." + quoteIdent(table) +
		" (" + strings.Join(colList, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
	return q, args
}

// buildDelete constructs `DELETE FROM db.tbl WHERE pk1=? AND
// pk2=…`. If the table has no PK, falls back to full-row WHERE
// (matching every non-null column to bind values) — slower but
// correct.
func buildDelete(target, table string, meta *tableMeta, row []any) (string, []any) {
	where, args := whereClause(meta, row)
	q := "DELETE FROM " + quoteIdent(target) + "." + quoteIdent(table) + " WHERE " + where + " LIMIT 1"
	return q, args
}

// buildUpdate constructs `UPDATE db.tbl SET col1=?,col2=? WHERE
// pk=?`. `before` carries the WHERE bindings; `after` carries the
// SET bindings. Generated columns are skipped in SET (MySQL error
// 3105) but remain valid in WHERE on the `before` row.
func buildUpdate(target, table string, meta *tableMeta, before, after []any) (string, []any) {
	idx := meta.writableIdx()
	setList := make([]string, len(idx))
	args := make([]any, 0, len(idx)+len(meta.PKCols))
	for j, i := range idx {
		setList[j] = quoteIdent(meta.Columns[i]) + "=?"
		args = append(args, after[i])
	}
	where, whereArgs := whereClause(meta, before)
	args = append(args, whereArgs...)
	q := "UPDATE " + quoteIdent(target) + "." + quoteIdent(table) +
		" SET " + strings.Join(setList, ", ") +
		" WHERE " + where + " LIMIT 1"
	return q, args
}

// whereClause builds the WHERE for UPDATE/DELETE. Prefers PK; falls
// back to a row-content match.
func whereClause(meta *tableMeta, row []any) (string, []any) {
	if len(meta.PKCols) > 0 {
		parts := make([]string, 0, len(meta.PKCols))
		args := make([]any, 0, len(meta.PKCols))
		for _, idx := range meta.PKCols {
			parts = append(parts, quoteIdent(meta.Columns[idx])+"=?")
			args = append(args, row[idx])
		}
		return strings.Join(parts, " AND "), args
	}
	// No PK — fall back to NULL-safe equality on every column.
	parts := make([]string, 0, len(meta.Columns))
	args := make([]any, 0, len(meta.Columns))
	for i, c := range meta.Columns {
		parts = append(parts, quoteIdent(c)+" <=> ?")
		args = append(args, row[i])
	}
	return strings.Join(parts, " AND "), args
}

// applyRowsForTarget runs the reconstructed SQL for one row event
// against a single target database. Errors per-row are logged and
// surfaced so the caller can decide whether to abort the batch.
func applyRowsForTarget(
	ctx context.Context,
	conn *sql.DB,
	target, table string,
	meta *tableMeta,
	et replication.EventType,
	rows [][]any,
) error {
	switch et {
	case replication.WRITE_ROWS_EVENTv0, replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
		for _, row := range rows {
			q, args := buildInsert(target, table, meta, row)
			if _, err := conn.ExecContext(ctx, q, args...); err != nil {
				slog.Warn("binlog dml insert", "target", target, "tbl", table, "err", err)
			}
		}
	case replication.DELETE_ROWS_EVENTv0, replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		for _, row := range rows {
			q, args := buildDelete(target, table, meta, row)
			if _, err := conn.ExecContext(ctx, q, args...); err != nil {
				slog.Warn("binlog dml delete", "target", target, "tbl", table, "err", err)
			}
		}
	case replication.UPDATE_ROWS_EVENTv0, replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
		// Update events ship rows in (before, after) pairs.
		for i := 0; i+1 < len(rows); i += 2 {
			before, after := rows[i], rows[i+1]
			q, args := buildUpdate(target, table, meta, before, after)
			if _, err := conn.ExecContext(ctx, q, args...); err != nil {
				slog.Warn("binlog dml update", "target", target, "tbl", table, "err", err)
			}
		}
	default:
		return fmt.Errorf("unhandled rows event: %s", et)
	}
	return nil
}
