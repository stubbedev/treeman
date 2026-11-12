package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/ident"
)

// CloneStrategy names the SnapshotCreate path actually taken. Surfaced
// to callers so the prepare layer can record which path ran (and how
// much faster it got).
type CloneStrategy string

const (
	// CloneStrategyPhysical used InnoDB transferable tablespaces — the
	// source's .ibd files were physically copied into the template's
	// data dir inside the container via `<engine> exec cp`. Avoids the
	// per-row `INSERT … SELECT` round-trips entirely.
	CloneStrategyPhysical CloneStrategy = "physical"
	// CloneStrategyLogical used the existing serial-DDL + parallel-
	// INSERT-SELECT loader. Always available; picked when the
	// preconditions for physical clone aren't met.
	CloneStrategyLogical CloneStrategy = "logical"
)

// ErrPhysicalCloneSkipped is returned by tryPhysicalSnapshotCreate when
// the preconditions for physical clone aren't met (no ContainerRef, no
// docker on PATH, non-InnoDB tables, etc.). Treated as a signal to fall
// back, not a hard failure.
type physicalSkippedError struct{ reason string }

func (e *physicalSkippedError) Error() string {
	return "physical clone skipped: " + e.reason
}

// tryPhysicalSnapshotCreate attempts an InnoDB transferable-tablespaces
// clone of source → template. Returns ok=true on success; ok=false with
// an *physicalSkippedError when the preconditions aren't met (caller
// silently falls back); ok=false with a real error when physical clone
// was attempted but failed (caller logs warn + falls back).
//
// The flow:
//
//  1. Probe @@datadir and the table list; require all InnoDB.
//  2. Recreate template DB; for every table SHOW CREATE on the template
//     then ALTER … DISCARD TABLESPACE so the freshly-created .ibd is
//     unlinked and the slot ready for an import.
//  3. FLUSH TABLES source.* FOR EXPORT on the same connection. This
//     freezes those tables read-only and writes a .cfg next to each
//     .ibd describing the tablespace layout.
//  4. `<engine> exec CID cp` each .ibd/.cfg inside the container, which
//     preserves the mysql:mysql ownership the server expects.
//  5. UNLOCK TABLES to release the source.
//  6. ALTER … IMPORT TABLESPACE on each table to attach the copied
//     files; mysql validates the .cfg against the discarded tablespace
//     and surfaces a clear error if the layouts diverged.
//
// On ANY error after the template CREATE, the partially-built template
// is dropped before returning so the caller's fallback logical clone
// can recreate it cleanly. MySQL's DROP DATABASE handles the case
// where the data dir contains files mysql doesn't know about (orphan
// .ibd left by a half-finished IMPORT) by walking the directory; we
// still want it explicit so the logical fallback never trips on
// "Schema directory already exists".
//
//nolint:cyclop // one linear orchestrator with explicit precondition checks; splitting just spreads the same branches across helpers without simplifying review
func (d *Driver) tryPhysicalSnapshotCreate(ctx context.Context, source, template string) (ok bool, err error) {
	if d.cfg.Container == "" && d.cfg.ComposeService == "" {
		return false, &physicalSkippedError{reason: "no ContainerRef on connections.mysql"}
	}
	opts := containerip.FromRef(d.cfg.ContainerRef, 0)
	cid, err := containerip.ContainerID(ctx, opts)
	if err != nil {
		return false, &physicalSkippedError{reason: "container not resolvable: " + err.Error()}
	}
	engineBin := opts.EngineBin()
	if _, err := exec.LookPath(engineBin); err != nil {
		return false, &physicalSkippedError{reason: engineBin + " binary not on PATH"}
	}

	var datadir string
	if err := d.DB.QueryRowContext(ctx, "SELECT @@datadir").Scan(&datadir); err != nil {
		return false, fmt.Errorf("read @@datadir: %w", err)
	}
	datadir = strings.TrimRight(datadir, "/")

	tables, err := listInnoDBTables(ctx, d.DB, source)
	if err != nil {
		return false, err
	}
	if len(tables) == 0 {
		// No base tables / non-InnoDB present — the logical path is
		// trivially fast for empty schemas and handles edge engines.
		return false, &physicalSkippedError{reason: "no InnoDB tables to transfer"}
	}

	qsource, err := ident.QuoteMySQL(source)
	if err != nil {
		return false, err
	}
	qtemplate, err := ident.QuoteMySQL(template)
	if err != nil {
		return false, err
	}
	if _, err := d.DB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qtemplate); err != nil {
		return false, err
	}
	if _, err := d.DB.ExecContext(ctx,
		"CREATE DATABASE "+qtemplate+d.createCharsetClause()); err != nil {
		return false, err
	}
	// Cleanup-on-failure guard: any error path past this point leaves
	// the template in a partially-built state (orphan .ibd files,
	// discarded tablespaces, etc.). The deferred cleanup fires when
	// the named return `ok` is false, so the caller's fallback logical
	// clone gets a clean slate. MySQL error 3678 ("Schema directory
	// already exists") is the symptom we're guarding against.
	defer func() {
		if ok {
			return
		}
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = errors.Join(err, cleanupPartialSchema(dropCtx, d, engineBin, cid, datadir, template, qtemplate))
	}()

	conn, releaseConn, err := sessionConn(ctx, d.DB)
	if err != nil {
		return false, err
	}
	defer releaseConn()
	// Disable FK / unique checks for the whole DDL pass: tables are
	// recreated in alphabetic order, so an `orders` table whose
	// CREATE references `products` would error out if FKs validate
	// during CREATE. The IMPORT TABLESPACE pass later carries the
	// same flag so referential integrity on the imported data isn't
	// re-validated row by row (it's preserved verbatim from source).
	if _, err := conn.ExecContext(ctx, "SET SESSION foreign_key_checks=0"); err != nil {
		return false, fmt.Errorf("disable foreign_key_checks: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION unique_checks=0"); err != nil {
		return false, fmt.Errorf("disable unique_checks: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "USE "+qtemplate); err != nil {
		return false, err
	}
	for _, t := range tables {
		if err := recreateAndDiscard(ctx, conn, qsource, t); err != nil {
			return false, err
		}
	}

	flushList := make([]string, 0, len(tables))
	for _, t := range tables {
		qt, qerr := ident.QuoteMySQL(t)
		if qerr != nil {
			return false, qerr
		}
		flushList = append(flushList, qsource+"."+qt)
	}
	if _, err := conn.ExecContext(ctx, "FLUSH TABLES "+strings.Join(flushList, ", ")+" FOR EXPORT"); err != nil {
		return false, fmt.Errorf("FLUSH TABLES FOR EXPORT: %w", err)
	}

	// Copy .ibd + .cfg from <datadir>/source/X.* → <datadir>/template/X.*
	// in a single docker exec (see copyTablespaces: per-file exec startup
	// otherwise dominates for many-table schemas). `--user mysql` is
	// load-bearing: the default exec user is root, so files would be
	// root-owned and opaque to the non-root mysqld for IMPORT TABLESPACE;
	// running cp as mysql makes them inherit mysql:mysql ownership.
	srcDir := datadir + "/" + source
	dstDir := datadir + "/" + template
	copyErr := copyTablespaces(ctx, engineBin, cid, srcDir, dstDir, tables)
	if _, err := conn.ExecContext(ctx, "UNLOCK TABLES"); err != nil && copyErr == nil {
		copyErr = fmt.Errorf("UNLOCK TABLES: %w", err)
	}
	if copyErr != nil {
		return false, copyErr
	}

	for _, t := range tables {
		qt, qerr := ident.QuoteMySQL(t)
		if qerr != nil {
			return false, qerr
		}
		if _, err := conn.ExecContext(ctx, "ALTER TABLE "+qt+" IMPORT TABLESPACE"); err != nil {
			return false, fmt.Errorf("IMPORT TABLESPACE %s: %w", t, err)
		}
	}
	return true, nil
}

// listInnoDBTables returns the BASE TABLE rows for `source`, requiring
// every entry to be InnoDB. Returns a non-error empty list when the
// schema is genuinely empty; returns a skip-error when a non-InnoDB
// engine shows up so the caller falls back to the logical path.
func listInnoDBTables(ctx context.Context, db *sql.DB, source string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			CONVERT(TABLE_NAME USING utf8mb4),
			COALESCE(ENGINE, '')
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`, source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name, eng string
		if err := rows.Scan(&name, &eng); err != nil {
			return nil, err
		}
		if !strings.EqualFold(eng, "InnoDB") {
			return nil, &physicalSkippedError{
				reason: fmt.Sprintf("table %s.%s uses %s engine; physical clone needs InnoDB", source, name, eng),
			}
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

// recreateAndDiscard SHOWs the source table's CREATE statement, runs
// it on the current connection's database (the template), then drops
// the freshly-created .ibd via DISCARD TABLESPACE so an IMPORT can
// attach the copied file later.
func recreateAndDiscard(ctx context.Context, conn *sql.Conn, qsource, t string) error {
	qt, err := ident.QuoteMySQL(t)
	if err != nil {
		return err
	}
	row := conn.QueryRowContext(ctx, "SHOW CREATE TABLE "+qsource+"."+qt)
	var name, createStmt string
	if err := row.Scan(&name, &createStmt); err != nil {
		return fmt.Errorf("SHOW CREATE %s.%s: %w", qsource, qt, err)
	}
	if _, err := conn.ExecContext(ctx, createStmt); err != nil {
		return fmt.Errorf("recreate %s: %w", qt, err)
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE "+qt+" DISCARD TABLESPACE"); err != nil {
		return fmt.Errorf("DISCARD TABLESPACE %s: %w", qt, err)
	}
	return nil
}

func cleanupPartialSchema(ctx context.Context, d *Driver, engineBin, cid, datadir, name, qname string) error {
	var errs []error
	if _, err := d.DB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qname); err != nil {
		errs = append(errs, fmt.Errorf("drop the partially built %s: %w", name, err))
	}
	if out, err := exec.CommandContext(ctx, engineBin, "exec", cid, "rm", "-rf", datadir+"/"+name).CombinedOutput(); err != nil {
		errs = append(errs, fmt.Errorf("remove the data directory of %s: %w: %s", name, err, strings.TrimSpace(string(out))))
	}
	return errors.Join(errs...)
}
