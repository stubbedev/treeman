package postgres

import (
	"context"
	"fmt"
	"slices"

	"github.com/stubbedev/treeman/internal/db/ident"
)

// rowCountSampleLimit is the verification sample size: exact COUNT(*)
// on the catalog's smallest non-empty tables is enough to distinguish
// a populated copy from a schema-only / partial one (#41) while
// staying cheap even on wide schemas.
const rowCountSampleLimit = 3

// nonEmptyTables lists up to rowCountSampleLimit of `db`'s tables —
// the ones pg_class estimates (reltuples) are smallest but non-empty.
// Estimates only PICK the sample; exact counts decide.
func (d *Driver) nonEmptyTables(ctx context.Context, db string) ([]string, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r' AND c.reltuples > 0
		ORDER BY c.reltuples ASC
		LIMIT $2`, db, rowCountSampleLimit)
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

// RowCountSample returns exact row counts for a small sample of `db`'s
// tables (see nonEmptyTables). An empty map means "no table the
// catalog believes non-empty" and is NOT a verification failure.
func (d *Driver) RowCountSample(ctx context.Context, db string) (map[string]int64, error) {
	if err := ident.ValidatePostgres(db); err != nil {
		return nil, err
	}
	tables, err := d.nonEmptyTables(ctx, db)
	if err != nil {
		return nil, err
	}
	return d.RowCountsFor(ctx, db, tables)
}

// RowCountsFor returns exact row counts for the NAMED tables in `db`.
// Capture verification uses it to count the SOURCE's sample tables in
// the COPY — by name, so the copy's own (possibly stale) catalog
// estimates never decide anything. Missing tables count as 0.
func (d *Driver) RowCountsFor(ctx context.Context, db string, tables []string) (map[string]int64, error) {
	if err := ident.ValidatePostgres(db); err != nil {
		return nil, err
	}
	schemaQ, err := ident.QuotePostgres(db)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(tables))
	for _, t := range slices.Clip(tables) {
		tableQ, err := ident.QuotePostgres(t)
		if err != nil {
			continue
		}
		var n int64
		if err := d.DB.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s.%s", schemaQ, tableQ)).Scan(&n); err != nil {
			return nil, fmt.Errorf("row count %s.%s: %w", db, t, err)
		}
		out[t] = n
	}
	return out, nil
}
