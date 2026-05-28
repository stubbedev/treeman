// Package dumpload streams a `.sql` dump into a target database
// over the wire. Statement-by-statement so we don't buffer the
// whole file in memory; quotes / backticks / `--` line comments /
// `/* */` block comments respected so an embedded `;` inside a
// string literal doesn't trigger a fake split.
//
// We use `database/sql`'s default ExecContext, which speaks MySQL's
// text protocol. That's enough for every statement mysqldump emits
// — including LOCK TABLES / SET PASSWORD / DELIMITER, which the
// prepared-statement protocol refuses to handle.
package dumpload

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
)

// LoadMySQL streams `dumpPath` into `targetDB` via the supplied
// *sql.DB. Returns the number of statements applied.
//
// Compression (gzip/zstd/bzip2/xz) is auto-detected from the file's
// magic bytes — extension is not consulted. Uses streamStatements
// internally so a 10GB dump is bounded by the largest statement
// size (typically ~64MB for mysqldump extended-inserts), not the
// file size.
func LoadMySQL(ctx context.Context, db *sql.DB, targetDB, dumpPath string) (uint64, error) {
	f, _, err := OpenDump(dumpPath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("USE `%s`", targetDB)); err != nil {
		return 0, fmt.Errorf("USE `%s`: %w", targetDB, err)
	}
	for _, s := range []string{
		"SET SESSION foreign_key_checks=0",
		"SET SESSION unique_checks=0",
		"SET SESSION sql_log_bin=0",
	} {
		_, _ = conn.ExecContext(ctx, s) // best-effort; some flags need SUPER
	}
	return streamStatements(ctx, f, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
}

// LoadPostgres mirrors LoadMySQL for pgx-backed databases.
// Compression auto-detection works identically.
func LoadPostgres(ctx context.Context, db *sql.DB, targetDB, dumpPath string) (uint64, error) {
	f, _, err := OpenDump(dumpPath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	// pg/`USE` doesn't exist; caller is expected to have opened db
	// against the right database (i.e. a DB-scoped connection).
	_ = targetDB
	return streamStatements(ctx, f, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
}

// streamStatements walks `r` and invokes onStmt for each `;`-
// terminated statement found at top level. Handles `--` line
// comments, `/* */` block comments, and `' " \“ quoting so an
// embedded `;` inside a string literal doesn't trigger a false
// split. Blank/whitespace-only statements are skipped.
//
// Memory ceiling is the largest single statement size — typically
// the extended INSERT chunk mysqldump emits (default 64MB). The
// file is consumed in 1MB chunks so even a 10GB dump never lives in
// RAM all at once.
//
// Scanning runs over each chunk slice with index access (no
// per-byte reader call), and the two-character `--` / `/*` / `*/`
// lookaheads are tracked as carry-over state (dashSeen / slashSeen /
// starSeen) so they survive chunk boundaries. This is a faithful,
// allocation-free replacement for the previous bufio.ReadByte +
// Peek/Discard loop — FuzzStreamStatementsParity pins the output to
// that reference implementation byte-for-byte.
func streamStatements(ctx context.Context, r io.Reader, onStmt func(stmt string) error) (uint64, error) {
	chunk := make([]byte, 1<<20)
	var buf bytes.Buffer
	var (
		inSingle, inDouble, inBacktick bool
		inLineComment, inBlockComment  bool
		starSeen, dashSeen, slashSeen  bool
		applied                        uint64
	)
	flush := func() error {
		stmt := buf.String()
		buf.Reset()
		if isBlank(stmt) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := onStmt(stmt); err != nil {
			return fmt.Errorf("apply dump stmt #%d: %w", applied, err)
		}
		applied++
		return nil
	}
	for {
		n, rerr := r.Read(chunk)
		for i := 0; i < n; i++ {
			b := chunk[i]
			switch {
			case inLineComment:
				if b == '\n' {
					inLineComment = false
				}
				continue
			case inBlockComment:
				if starSeen && b == '/' {
					inBlockComment = false
					starSeen = false
					continue
				}
				starSeen = b == '*'
				continue
			case inSingle:
				buf.WriteByte(b)
				if b == '\'' {
					inSingle = false
				}
				continue
			case inDouble:
				buf.WriteByte(b)
				if b == '"' {
					inDouble = false
				}
				continue
			case inBacktick:
				buf.WriteByte(b)
				if b == '`' {
					inBacktick = false
				}
				continue
			}
			// Top-level (not in a quote or comment). Resolve a pending
			// `-` / `/` from the previous byte first: it either forms a
			// comment opener or gets written verbatim before we handle b.
			if dashSeen {
				dashSeen = false
				if b == '-' {
					inLineComment = true
					continue
				}
				buf.WriteByte('-')
			} else if slashSeen {
				slashSeen = false
				if b == '*' {
					inBlockComment = true
					continue
				}
				buf.WriteByte('/')
			}
			switch b {
			case '-':
				dashSeen = true
			case '/':
				slashSeen = true
			case '\'':
				inSingle = true
				buf.WriteByte(b)
			case '"':
				inDouble = true
				buf.WriteByte(b)
			case '`':
				inBacktick = true
				buf.WriteByte(b)
			case ';':
				if err := flush(); err != nil {
					return applied, err
				}
			default:
				buf.WriteByte(b)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return applied, rerr
		}
	}
	// A `-` or `/` pending at EOF was a literal trailing byte, not a
	// comment opener — write it before the final flush (mirrors the
	// byte-by-byte path, which wrote it once Peek returned empty).
	if dashSeen {
		buf.WriteByte('-')
	}
	if slashSeen {
		buf.WriteByte('/')
	}
	// Trailing statement without a closing `;` (rare but tolerated).
	if err := flush(); err != nil {
		return applied, err
	}
	return applied, nil
}

func isBlank(s string) bool {
	for _, c := range s {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}
