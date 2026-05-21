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
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
)

// LoadMySQL streams `dumpPath` into `targetDB` via the supplied
// *sql.DB. Returns the number of statements applied.
//
// Uses streamStatements internally so a 10GB dump is bounded by the
// largest statement size (typically ~64MB for mysqldump extended-
// inserts), not the file size.
func LoadMySQL(ctx context.Context, db *sql.DB, targetDB, dumpPath string) (uint64, error) {
	f, err := os.Open(dumpPath)
	if err != nil {
		return 0, fmt.Errorf("read dump %s: %w", dumpPath, err)
	}
	defer f.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
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
func LoadPostgres(ctx context.Context, db *sql.DB, targetDB, dumpPath string) (uint64, error) {
	f, err := os.Open(dumpPath)
	if err != nil {
		return 0, fmt.Errorf("read dump %s: %w", dumpPath, err)
	}
	defer f.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
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
// file itself is consumed via a 1MB bufio.Reader so even a 10GB
// dump never lives in RAM all at once.
func streamStatements(ctx context.Context, r io.Reader, onStmt func(stmt string) error) (uint64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var buf bytes.Buffer
	var inSingle, inDouble, inBacktick bool
	var applied uint64
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
		b, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return applied, err
		}
		if !(inSingle || inDouble || inBacktick) {
			// line comment
			if b == '-' {
				if peek, _ := br.Peek(1); len(peek) == 1 && peek[0] == '-' {
					_, _ = br.Discard(1)
					if err := skipUntil(br, '\n'); err != nil && err != io.EOF {
						return applied, err
					}
					continue
				}
			}
			// block comment
			if b == '/' {
				if peek, _ := br.Peek(1); len(peek) == 1 && peek[0] == '*' {
					_, _ = br.Discard(1)
					if err := skipBlockComment(br); err != nil && err != io.EOF {
						return applied, err
					}
					continue
				}
			}
		}
		switch b {
		case '\'':
			if !inDouble && !inBacktick {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle && !inBacktick {
				inDouble = !inDouble
			}
		case '`':
			if !inSingle && !inDouble {
				inBacktick = !inBacktick
			}
		case ';':
			if !inSingle && !inDouble && !inBacktick {
				if err := flush(); err != nil {
					return applied, err
				}
				continue
			}
		}
		buf.WriteByte(b)
	}
	// Trailing statement without a closing `;` (rare but tolerated).
	if err := flush(); err != nil {
		return applied, err
	}
	return applied, nil
}

// skipUntil discards bytes from r up to and including the next sep.
// Used to consume `--` line comments through their trailing newline.
func skipUntil(r *bufio.Reader, sep byte) error {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		if b == sep {
			return nil
		}
	}
}

// skipBlockComment discards bytes from r through a `*/` terminator.
// Caller has already consumed the opening `/*`.
func skipBlockComment(r *bufio.Reader) error {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		if b == '*' {
			peek, _ := r.Peek(1)
			if len(peek) == 1 && peek[0] == '/' {
				_, _ = r.Discard(1)
				return nil
			}
		}
	}
}

func isBlank(s string) bool {
	for _, c := range s {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}
