package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/stubbedev/treeman/internal/db/ident"
)

// WriteWatermark returns a token for `db` that stays equal between two
// calls only if neither a row write nor a schema change reached `db`
// and the server did not restart in between, so the caller can skip a
// redundant capture or copy. An error returns "" so the caller declines
// to skip work.
//
// The token joins three parts, each covering what the others miss:
//
//  1. A row-write counter: SUM(COUNT_WRITE) over `db`'s tables from
//     performance_schema.table_io_waits_summary_by_table when the
//     table-io instrument and global consumer are verified ENABLED
//     (otherwise COUNT_WRITE pins at 0), else the server-wide
//     Innodb_rows_{inserted,updated,deleted}. Tagged "ps:"/"ir:" so a
//     mode flip never compares equal. Neither is monotonic: both reset
//     on restart, and a table's COUNT_WRITE vanishes or restarts at 0
//     when the table is dropped, renamed or altered.
//  2. The server's boot: when this mysqld logged its start
//     (performance_schema.error_log), so a restart that zeroed the
//     counters changes the token.
//  3. A digest of `db`'s schema (see schemaDigest), so DDL that resets
//     or bypasses the counters changes the token.
//
// What remains undetected is DDL that resets a table's counter and is
// then exactly undone (ADD INDEX then DROP INDEX of the same index)
// after row writes the counter already held.
func (d *Driver) WriteWatermark(ctx context.Context, db string) (string, error) {
	if err := ident.ValidateMySQL(db); err != nil {
		return "", err
	}
	writes, err := d.rowWriteCounter(ctx, db)
	if err != nil {
		return "", fmt.Errorf("write watermark of %s: %w", db, err)
	}
	boot, err := d.serverBoot(ctx)
	if err != nil {
		return "", fmt.Errorf("write watermark of %s: %w", db, err)
	}
	schema, err := d.schemaDigest(ctx, db)
	if err != nil {
		return "", fmt.Errorf("write watermark of %s: %w", db, err)
	}
	return writes + "@" + boot + "#" + schema, nil
}

func (d *Driver) rowWriteCounter(ctx context.Context, db string) (string, error) {
	if tracked, _ := d.perfSchemaWritesTracked(ctx); tracked {
		var sum sql.NullInt64
		if err := d.DB.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(COUNT_WRITE), 0)
			 FROM performance_schema.table_io_waits_summary_by_table
			 WHERE OBJECT_SCHEMA = ?`, db).Scan(&sum); err == nil {
			return "ps:" + db + ":" + strconv.FormatInt(sum.Int64, 10), nil
		}
	}
	return d.globalRowWatermark(ctx)
}

// perfSchemaWritesTracked reports whether table-level write counts are
// actually being recorded — i.e. the table-io instrument AND the global
// instrumentation consumer are both ENABLED. If either is off (or
// performance_schema is disabled entirely, surfacing as a query error),
// COUNT_WRITE is untrustworthy and the caller must NOT use the per-db
// path.
func (d *Driver) perfSchemaWritesTracked(ctx context.Context) (bool, error) {
	var inst, cons sql.NullString
	if err := d.DB.QueryRowContext(ctx, `
		SELECT
			(SELECT ENABLED FROM performance_schema.setup_instruments
			   WHERE NAME = 'wait/io/table/sql/handler' LIMIT 1),
			(SELECT ENABLED FROM performance_schema.setup_consumers
			   WHERE NAME = 'global_instrumentation' LIMIT 1)`).Scan(&inst, &cons); err != nil {
		return false, err
	}
	return inst.Valid && inst.String == "YES" && cons.Valid && cons.String == "YES", nil
}

// globalRowWatermark is the always-available server-wide fallback: the
// sum of the Innodb_rows_* counters from SHOW GLOBAL STATUS.
func (d *Driver) globalRowWatermark(ctx context.Context) (string, error) {
	rows, err := d.DB.QueryContext(ctx,
		`SHOW GLOBAL STATUS WHERE Variable_name IN
		 ('Innodb_rows_inserted','Innodb_rows_updated','Innodb_rows_deleted')`)
	if err != nil {
		return "", fmt.Errorf("read the InnoDB row counters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sum int64
	for rows.Next() {
		var name string
		var val sql.NullInt64
		if err := rows.Scan(&name, &val); err != nil {
			return "", fmt.Errorf("read the InnoDB row counters: %w", err)
		}
		sum += val.Int64
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read the InnoDB row counters: %w", err)
	}
	return "ir:" + strconv.FormatInt(sum, 10), nil
}

// errServerBootUnknown means performance_schema.error_log no longer
// holds the server's start message, so a restart cannot be told apart.
var errServerBootUnknown = errors.New("the server's start message is not in performance_schema.error_log")

// serverBoot returns when the running mysqld logged its start
// (MY-010116, "starting as process"), to microseconds.
func (d *Driver) serverBoot(ctx context.Context) (string, error) {
	var logged sql.NullString
	if err := d.DB.QueryRowContext(ctx,
		`SELECT CAST(MAX(LOGGED) AS CHAR) FROM performance_schema.error_log WHERE ERROR_CODE = 'MY-010116'`,
	).Scan(&logged); err != nil {
		return "", fmt.Errorf("read the server's start from performance_schema.error_log: %w", err)
	}
	if !logged.Valid {
		return "", errServerBootUnknown
	}
	return logged.String, nil
}

// schemaDigest hashes what DDL changes in schema: each InnoDB table's
// dictionary identity (a new TABLE_ID on CREATE, TRUNCATE and table
// rebuilds), its index ids, every column definition, and the views,
// triggers, routines and events with their definitions. Names are
// [A-Za-z0-9_], so InnoDB's "<schema>/<table>" names of schema are
// exactly the range ["<schema>/", "<schema>0").
func (d *Driver) schemaDigest(ctx context.Context, schema string) (string, error) {
	lo, hi := schema+"/", schema+"0"
	rows, err := d.DB.QueryContext(ctx, `
		SELECT 'T', CONVERT(t.NAME USING utf8mb4), CAST(t.TABLE_ID AS CHAR), CAST(t.N_COLS AS CHAR)
		  FROM information_schema.INNODB_TABLES t WHERE t.NAME >= ? AND t.NAME < ?
		UNION ALL
		SELECT 'I', CONVERT(t.NAME USING utf8mb4), CAST(i.INDEX_ID AS CHAR), CONVERT(i.NAME USING utf8mb4)
		  FROM information_schema.INNODB_TABLES t
		  JOIN information_schema.INNODB_INDEXES i ON i.TABLE_ID = t.TABLE_ID
		 WHERE t.NAME >= ? AND t.NAME < ?
		UNION ALL
		SELECT 'C', CONVERT(TABLE_NAME USING utf8mb4),
		       CONCAT(LPAD(ORDINAL_POSITION, 5, '0'), ':', CONVERT(COLUMN_NAME USING utf8mb4)),
		       CONCAT_WS('|', CONVERT(COLUMN_TYPE USING utf8mb4), IS_NULLABLE,
		                 COALESCE(CONVERT(COLUMN_DEFAULT USING utf8mb4), '<null>'), CONVERT(EXTRA USING utf8mb4))
		  FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ?
		UNION ALL
		SELECT 'V', CONVERT(TABLE_NAME USING utf8mb4), '', CONVERT(VIEW_DEFINITION USING utf8mb4)
		  FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?
		UNION ALL
		SELECT 'G', CONVERT(TRIGGER_NAME USING utf8mb4), CAST(CREATED AS CHAR), CONVERT(ACTION_STATEMENT USING utf8mb4)
		  FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?
		UNION ALL
		SELECT 'R', CONVERT(ROUTINE_NAME USING utf8mb4), CAST(LAST_ALTERED AS CHAR), CONVERT(ROUTINE_DEFINITION USING utf8mb4)
		  FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?
		UNION ALL
		SELECT 'E', CONVERT(EVENT_NAME USING utf8mb4), CAST(LAST_ALTERED AS CHAR), CONVERT(EVENT_DEFINITION USING utf8mb4)
		  FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?
		ORDER BY 1, 2, 3`,
		lo, hi, lo, hi, schema, schema, schema, schema, schema)
	if err != nil {
		return "", fmt.Errorf("read the schema of %s: %w", schema, err)
	}
	defer func() { _ = rows.Close() }()
	h := sha256.New()
	var cols [4]sql.NullString
	for rows.Next() {
		if err := rows.Scan(&cols[0], &cols[1], &cols[2], &cols[3]); err != nil {
			return "", fmt.Errorf("read the schema of %s: %w", schema, err)
		}
		for _, c := range cols {
			field := "\x01"
			if c.Valid {
				field = strconv.Itoa(len(c.String)) + ":" + c.String
			}
			_, _ = h.Write([]byte(field))
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read the schema of %s: %w", schema, err)
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}
