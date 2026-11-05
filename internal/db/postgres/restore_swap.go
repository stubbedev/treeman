package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stubbedev/treeman/internal/db/ident"
	"github.com/stubbedev/treeman/internal/db/restorestage"
)

const (
	sqlStateObjectInUse        = "55006"
	sqlStateDuplicateDatabase  = "42P04"
	sqlStateInvalidCatalogName = "3D000"
)

// replaceDatabase makes `from` the database named `to`. In one
// transaction the current `to`, if any, is renamed to `retired` and
// `from` is renamed to `to`; ALTER DATABASE … RENAME may run inside a
// transaction block, so other sessions see either the old or the new
// database under `to`, never a gap in which a client could create it.
// Postgres refuses to rename a database other sessions are connected
// to, so a live connection to `to` fails the swap rather than being
// cut.
func (d *Driver) replaceDatabase(ctx context.Context, from, to, retired string) error {
	qfrom, err := ident.QuotePostgres(from)
	if err != nil {
		return err
	}
	qto, err := ident.QuotePostgres(to)
	if err != nil {
		return err
	}
	qretired, err := ident.QuotePostgres(retired)
	if err != nil {
		return err
	}
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin swapping %s into %s: %w", from, to, err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", to).Scan(&exists); err != nil {
		return fmt.Errorf("look up %s: %w", to, err)
	}
	if exists {
		if _, err := tx.ExecContext(ctx, "ALTER DATABASE "+qto+" RENAME TO "+qretired); err != nil {
			return swapError(to, fmt.Errorf("rename %s to %s: %w", to, retired, err))
		}
	}
	if _, err := tx.ExecContext(ctx, "ALTER DATABASE "+qfrom+" RENAME TO "+qto); err != nil {
		return swapError(to, fmt.Errorf("rename %s to %s: %w", from, to, err))
	}
	if err := tx.Commit(); err != nil {
		return swapError(to, fmt.Errorf("commit swapping %s into %s: %w", from, to, err))
	}
	return nil
}

// swapError wraps a failed swap statement, naming what another client
// did to target when the server error says so.
func swapError(target string, err error) error {
	pe, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return err
	}
	switch pe.Code {
	case sqlStateObjectInUse:
		return fmt.Errorf("other sessions are connected to %s, so the restore cannot replace it: %w", target, err)
	case sqlStateDuplicateDatabase, sqlStateInvalidCatalogName:
		return fmt.Errorf("another client created or dropped %s while it was being restored: %w", target, err)
	default:
		return err
	}
}

// dropDatabases drops each named database if it exists.
func (d *Driver) dropDatabases(ctx context.Context, names ...string) error {
	var errs []error
	for _, n := range names {
		if err := d.DropDatabase(ctx, n); err != nil {
			errs = append(errs, fmt.Errorf("drop %s: %w", n, err))
		}
	}
	return errors.Join(errs...)
}

// reapStaleRestoreStages drops the restore stages a crashed run left
// behind. It never fails the restore that runs it: a leftover that
// cannot be dropped is logged and retried by the next restore.
func (d *Driver) reapStaleRestoreStages(ctx context.Context, now time.Time) {
	names, err := d.ListMatching(ctx, restorestage.Prefix)
	if err != nil {
		slog.Warn("postgres: list leftover restore stages", "prefix", restorestage.Prefix, "err", err)
		return
	}
	stale := restorestage.Stale(names, now)
	if len(stale) == 0 {
		return
	}
	if err := d.dropDatabases(ctx, stale...); err != nil {
		slog.Warn("postgres: drop leftover restore stages", "databases", stale, "err", err)
	}
}

// beginRestoreStage reaps the stages crashed runs left behind and
// returns the names for a new one.
func (d *Driver) beginRestoreStage(ctx context.Context) restorestage.Stage {
	now := time.Now()
	d.reapStaleRestoreStages(ctx, now)
	return restorestage.New(now)
}

// dropRestoreStage drops the named stage databases even when ctx is
// already cancelled, joining any failure into *errp.
func (d *Driver) dropRestoreStage(ctx context.Context, errp *error, from, to string, names ...string) {
	dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if derr := d.dropDatabases(dropCtx, names...); derr != nil {
		*errp = errors.Join(*errp, fmt.Errorf("restore %s into %s: %w", from, to, derr))
	}
}
