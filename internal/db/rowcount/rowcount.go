// Package rowcount is the shared implementation behind the mysql and
// postgres RowCountSample / RowCountsFor driver methods: exact row
// counts for a small sample of a database's tables, used to verify
// branch_scoped captures/resumes against schema-only corruption (#41).
// Catalog estimates only PICK the sample; exact counts decide.
package rowcount

import (
	"context"
	"fmt"
)

// SampleLimit is the verification sample size per namespace.
const SampleLimit = 3

// CountOf counts one table (db.table) in the target engine. The
// callback owns quoting/identifier validation.
type CountOf func(ctx context.Context, db, table string) (int64, error)

// Sample returns exact row counts for up to SampleLimit of `db`'s
// tables named by listTables (the driver's catalog query for "smallest
// non-empty"). An empty result is NOT a verification failure — it means
// the catalog believes the namespace has no rows.
func Sample(
	ctx context.Context,
	listTables func(ctx context.Context, db string) ([]string, error),
	countOf CountOf,
	db string,
) (map[string]int64, error) {
	tables, err := listTables(ctx, db)
	if err != nil {
		return nil, err
	}
	return CountsFor(ctx, countOf, db, tables)
}

// CountsFor returns exact row counts for the NAMED tables in `db` —
// by name, so the copy's own (possibly stale) catalog estimates never
// decide anything. Missing tables count as 0.
func CountsFor(ctx context.Context, countOf CountOf, db string, tables []string) (map[string]int64, error) {
	out := make(map[string]int64, len(tables))
	for _, t := range tables {
		n, err := countOf(ctx, db, t)
		if err != nil {
			return nil, fmt.Errorf("row count %s.%s: %w", db, t, err)
		}
		out[t] = n
	}
	return out, nil
}
