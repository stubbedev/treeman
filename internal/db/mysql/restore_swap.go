package mysql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/db/ident"
	"github.com/stubbedev/treeman/internal/db/restorestage"
)

const (
	erBadDB            uint16 = 1049
	erTableExists      uint16 = 1050
	erNoSuchTable      uint16 = 1146
	erTrgInWrongSchema uint16 = 1435
)

// restoreViaSwap replaces `target` with a clone of `template` without
// `target` ever ceasing to exist. A test runner that creates `target`
// when it is missing (Laravel's parallel testing does, for every
// `<db>_test_N`) therefore never races the restore into the database.
//
// build clones template's base tables into a private incoming schema;
// swapInto then moves target's tables out and the clone's tables in
// with one atomic RENAME TABLE, and the views and triggers are replayed
// into target from template. Both private schemas are dropped on every
// return path; a crashed run's leftovers are reaped by a later restore
// once they are restorestage.MaxAge old.
func (d *Driver) restoreViaSwap(
	ctx context.Context,
	template, target string,
	build func(ctx context.Context, source, dest string) error,
) (err error) {
	if err := ident.ValidateMySQL(template); err != nil {
		return err
	}
	if err := ident.ValidateMySQL(target); err != nil {
		return err
	}
	now := time.Now()
	d.reapStaleRestoreStages(ctx, now)
	stage := restorestage.New(now)
	defer func() {
		dropCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if derr := d.dropSchemas(dropCtx, stage.Incoming, stage.Retired); derr != nil {
			err = errors.Join(err, fmt.Errorf("restore %s into %s: %w", template, target, derr))
		}
	}()
	if err := build(ctx, template, stage.Incoming); err != nil {
		return fmt.Errorf("restore %s into %s: clone into %s: %w", template, target, stage.Incoming, err)
	}
	if err := d.swapInto(ctx, stage, target); err != nil {
		return fmt.Errorf("restore %s into %s: %w", template, target, err)
	}
	if err := d.cloneDictionaryObjects(ctx, template, target); err != nil {
		return fmt.Errorf("restore %s into %s: %w", template, target, err)
	}
	return nil
}

// swapInto makes stage.Incoming's base tables the base tables of
// target. target is created when missing and keeps existing throughout.
// Its views, triggers, routines and events are dropped first: triggers
// pin a table to its schema (ER_TRG_IN_WRONG_SCHEMA) and views cannot
// move between schemas, and the caller replays the template's own.
// One RENAME TABLE, which MySQL applies atomically and left to right,
// then moves every table of target into stage.Retired and every table
// of stage.Incoming into target. Foreign keys follow their renamed
// parents, so the clone's constraints end up pointing into target.
func (d *Driver) swapInto(ctx context.Context, stage restorestage.Stage, target string) error {
	qtarget, err := ident.QuoteMySQL(target)
	if err != nil {
		return err
	}
	qretired, err := ident.QuoteMySQL(stage.Retired)
	if err != nil {
		return err
	}
	if err := d.EnsureDB(ctx, target); err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx, "ALTER DATABASE "+qtarget+d.createCharsetClause()); err != nil {
		return fmt.Errorf("ALTER DATABASE %s: %w", qtarget, err)
	}
	if _, err := d.DB.ExecContext(ctx, "CREATE DATABASE "+qretired+d.createCharsetClause()); err != nil {
		return fmt.Errorf("CREATE DATABASE %s: %w", qretired, err)
	}

	objs, err := d.listDictionaryObjects(ctx, target)
	if err != nil {
		return err
	}
	if len(objs) > 0 {
		script, err := dropDictionaryScript(target, objs)
		if err != nil {
			return err
		}
		if _, err := d.DB.ExecContext(ctx, script); err != nil {
			return swapError(target, fmt.Errorf("drop the views, triggers, routines and events of %s: %w", qtarget, err))
		}
	}

	current, err := d.baseTableNames(ctx, target)
	if err != nil {
		return err
	}
	incoming, err := d.baseTableNames(ctx, stage.Incoming)
	if err != nil {
		return err
	}
	if len(current)+len(incoming) > 0 {
		stmt, err := renameSwapStatement(target, stage, current, incoming)
		if err != nil {
			return err
		}
		if _, err := d.DB.ExecContext(ctx, stmt); err != nil {
			return swapError(target, fmt.Errorf("swap the tables of %s into %s: %w", stage.Incoming, qtarget, err))
		}
	}

	after, err := d.baseTableNames(ctx, target)
	if err != nil {
		return err
	}
	if extra, missing := diffNames(after, incoming); len(extra)+len(missing) > 0 {
		return fmt.Errorf(
			"another client created or dropped tables in %s while it was being restored: unexpected %v, missing %v",
			target, extra, missing)
	}
	return nil
}

// swapError wraps a failed swap statement. A server error that can only
// mean target's objects changed between listing and swapping — a table
// that appeared, vanished, gained a trigger, or a dropped database —
// says that another client touched target during the restore.
func swapError(target string, err error) error {
	if hasErrorNumber(err, erBadDB, erTableExists, erNoSuchTable, erTrgInWrongSchema) {
		return fmt.Errorf("another client created or dropped objects in %s while it was being restored: %w", target, err)
	}
	return err
}

// renameSwapStatement renders the single RENAME TABLE that moves every
// table in current out of target into stage.Retired and then every
// table in incoming from stage.Incoming into target. The moves out come
// first because MySQL renames left to right.
func renameSwapStatement(target string, stage restorestage.Stage, current, incoming []string) (string, error) {
	qtarget, err := ident.QuoteMySQL(target)
	if err != nil {
		return "", err
	}
	qretired, err := ident.QuoteMySQL(stage.Retired)
	if err != nil {
		return "", err
	}
	qincoming, err := ident.QuoteMySQL(stage.Incoming)
	if err != nil {
		return "", err
	}
	moves := make([]string, 0, len(current)+len(incoming))
	for _, t := range current {
		qt, err := ident.QuoteMySQL(t)
		if err != nil {
			return "", err
		}
		moves = append(moves, qtarget+"."+qt+" TO "+qretired+"."+qt)
	}
	for _, t := range incoming {
		qt, err := ident.QuoteMySQL(t)
		if err != nil {
			return "", err
		}
		moves = append(moves, qincoming+"."+qt+" TO "+qtarget+"."+qt)
	}
	return "RENAME TABLE " + strings.Join(moves, ", "), nil
}

// diffNames returns the names only in got (extra) and only in want
// (missing), each in the order of its input.
func diffNames(got, want []string) (extra, missing []string) {
	inGot := make(map[string]bool, len(got))
	for _, n := range got {
		inGot[n] = true
	}
	inWant := make(map[string]bool, len(want))
	for _, n := range want {
		inWant[n] = true
		if !inGot[n] {
			missing = append(missing, n)
		}
	}
	for _, n := range got {
		if !inWant[n] {
			extra = append(extra, n)
		}
	}
	return extra, missing
}

// baseTableNames lists the base tables of schema in name order.
func (d *Driver) baseTableNames(ctx context.Context, schema string) ([]string, error) {
	tables, _, err := d.listTablesAndViews(ctx, schema)
	if err != nil {
		return nil, fmt.Errorf("list the tables of %s: %w", schema, err)
	}
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.name
	}
	return names, nil
}

// dictionaryKind is the DROP keyword of an object that lives only in
// the data dictionary of a schema.
type dictionaryKind string

const (
	dictionaryTrigger   dictionaryKind = "TRIGGER"
	dictionaryView      dictionaryKind = "VIEW"
	dictionaryProcedure dictionaryKind = "PROCEDURE"
	dictionaryFunction  dictionaryKind = "FUNCTION"
	dictionaryEvent     dictionaryKind = "EVENT"
)

func parseDictionaryKind(s string) (dictionaryKind, error) {
	switch k := dictionaryKind(s); k {
	case dictionaryTrigger, dictionaryView, dictionaryProcedure, dictionaryFunction, dictionaryEvent:
		return k, nil
	default:
		return "", fmt.Errorf("unknown dictionary object kind %q", s)
	}
}

type dictionaryObject struct {
	kind dictionaryKind
	name string
}

// listDictionaryObjects returns every trigger, view, stored routine and
// event of schema — everything DROP DATABASE used to remove besides the
// base tables the swap moves out.
func (d *Driver) listDictionaryObjects(ctx context.Context, schema string) ([]dictionaryObject, error) {
	rows, err := d.DB.QueryContext(ctx, `
		SELECT 'TRIGGER', CONVERT(TRIGGER_NAME USING utf8mb4) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?
		UNION ALL
		SELECT 'VIEW', CONVERT(TABLE_NAME USING utf8mb4) FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?
		UNION ALL
		SELECT CONVERT(ROUTINE_TYPE USING utf8mb4), CONVERT(ROUTINE_NAME USING utf8mb4) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?
		UNION ALL
		SELECT 'EVENT', CONVERT(EVENT_NAME USING utf8mb4) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?
		ORDER BY 1, 2`, schema, schema, schema, schema)
	if err != nil {
		return nil, fmt.Errorf("list the views, triggers, routines and events of %s: %w", schema, err)
	}
	defer func() { _ = rows.Close() }()
	var out []dictionaryObject
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return nil, fmt.Errorf("list the views, triggers, routines and events of %s: %w", schema, err)
		}
		k, err := parseDictionaryKind(kind)
		if err != nil {
			return nil, fmt.Errorf("list the views, triggers, routines and events of %s: %w", schema, err)
		}
		out = append(out, dictionaryObject{kind: k, name: name})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the views, triggers, routines and events of %s: %w", schema, err)
	}
	return out, nil
}

// dropDictionaryScript renders one multi-statement batch dropping every
// object in objs from schema.
func dropDictionaryScript(schema string, objs []dictionaryObject) (string, error) {
	qschema, err := ident.QuoteMySQL(schema)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, o := range objs {
		qn, err := ident.QuoteMySQL(o.name)
		if err != nil {
			return "", err
		}
		b.WriteString("DROP ")
		b.WriteString(string(o.kind))
		b.WriteString(" IF EXISTS ")
		b.WriteString(qschema)
		b.WriteByte('.')
		b.WriteString(qn)
		b.WriteString(";\n")
	}
	return b.String(), nil
}

// dropSchemas drops each named schema if it exists, with foreign key
// checks off so a retired table still referenced from another schema
// cannot block the drop.
func (d *Driver) dropSchemas(ctx context.Context, names ...string) error {
	conn, release, err := sessionConn(ctx, d.DB)
	if err != nil {
		return err
	}
	defer release()
	if _, err := conn.ExecContext(ctx, "SET SESSION foreign_key_checks=0"); err != nil {
		return fmt.Errorf("disable foreign_key_checks: %w", err)
	}
	var errs []error
	for _, n := range names {
		qn, err := ident.QuoteMySQL(n)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qn); err != nil {
			errs = append(errs, fmt.Errorf("DROP DATABASE %s: %w", qn, err))
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
		slog.Warn("mysql: list leftover restore stages", "prefix", restorestage.Prefix, "err", err)
		return
	}
	stale := restorestage.Stale(names, now)
	if len(stale) == 0 {
		return
	}
	if err := d.dropSchemas(ctx, stale...); err != nil {
		slog.Warn("mysql: drop leftover restore stages", "schemas", stale, "err", err)
	}
}
