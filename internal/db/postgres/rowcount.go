package postgres

import (
	"context"
	"fmt"

	"github.com/stubbedev/treeman/internal/db/ident"
	"github.com/stubbedev/treeman/internal/db/rowcount"
)

// nonEmptyTables lists up to rowcount.SampleLimit of `db`'s tables —
// the ones pg_class estimates (reltuples) are smallest but non-empty.
// Estimates only PICK the sample; exact counts decide.
func (d *Driver) nonEmptyTables(ctx context.Context, db string) ([]string, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r' AND c.reltuples > 0
		ORDER BY c.reltuples ASC
		LIMIT $2`, db, rowcount.SampleLimit)
	if err != nil {
		return nil, fmt.Errorf("row-count sample list %s: %w", db, err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

// countOf counts one table with identifier validation + quoting.
func (d *Driver) countOf(ctx context.Context, db, table string) (int64, error) {
	schemaQ, err := ident.QuotePostgres(db)
	if err != nil {
		return 0, err
	}
	tableQ, err := ident.QuotePostgres(table)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := d.DB.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s.%s", schemaQ, tableQ)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// RowCountSample returns exact row counts for a small sample of `db`'s
// tables (see nonEmptyTables). Capture/restore verification compares
// the source's sample against the copy's; an empty map means "no table
// the catalog believes non-empty" and is NOT a verification failure.
func (d *Driver) RowCountSample(ctx context.Context, db string) (map[string]int64, error) {
	if err := ident.ValidatePostgres(db); err != nil {
		return nil, err
	}
	return rowcount.Sample(ctx, d.nonEmptyTables, d.countOf, db)
}

// RowCountsFor returns exact row counts for the NAMED tables in `db`.
// Capture verification uses it to count the SOURCE's sample tables in
// the COPY — by name, so the copy's own catalog estimates never decide.
func (d *Driver) RowCountsFor(ctx context.Context, db string, tables []string) (map[string]int64, error) {
	if err := ident.ValidatePostgres(db); err != nil {
		return nil, err
	}
	return rowcount.CountsFor(ctx, d.countOf, db, tables)
}
