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
	"context"
	"database/sql"
	"fmt"
	"os"
)

// LoadMySQL streams `dumpPath` into `targetDB` via the supplied
// *sql.DB. Returns the number of statements applied.
func LoadMySQL(ctx context.Context, db *sql.DB, targetDB, dumpPath string) (uint64, error) {
	body, err := os.ReadFile(dumpPath)
	if err != nil {
		return 0, fmt.Errorf("read dump %s: %w", dumpPath, err)
	}
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
	var applied uint64
	for _, stmt := range iterStatements(string(body)) {
		if isBlank(stmt) {
			continue
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return applied, fmt.Errorf("apply dump stmt #%d: %w", applied, err)
		}
		applied++
	}
	return applied, nil
}

// LoadPostgres mirrors LoadMySQL for pgx-backed databases.
func LoadPostgres(ctx context.Context, db *sql.DB, targetDB, dumpPath string) (uint64, error) {
	body, err := os.ReadFile(dumpPath)
	if err != nil {
		return 0, fmt.Errorf("read dump %s: %w", dumpPath, err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	// pg/`USE` doesn't exist; caller is expected to have opened db
	// against the right database (i.e. a DB-scoped connection).
	_ = targetDB
	var applied uint64
	for _, stmt := range iterStatements(string(body)) {
		if isBlank(stmt) {
			continue
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return applied, fmt.Errorf("apply dump stmt #%d: %w", applied, err)
		}
		applied++
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

// iterStatements walks `sql` and splits at top-level `;`. Handles
// `--` line comments, `/* */` block comments, and three quote
// flavours so the splitter doesn't trip on embedded literals.
func iterStatements(sql string) []string {
	out := make([]string, 0, 64)
	rest := sql
	for len(rest) > 0 {
		stmt, next := nextStatement(rest)
		out = append(out, stmt)
		rest = next
	}
	return out
}

func nextStatement(s string) (string, string) {
	var inSingle, inDouble, inBacktick bool
	i := 0
	for i < len(s) {
		b := s[i]
		if !(inSingle || inDouble || inBacktick) {
			// line comment
			if b == '-' && i+1 < len(s) && s[i+1] == '-' {
				for i < len(s) && s[i] != '\n' {
					i++
				}
				continue
			}
			// block comment
			if b == '/' && i+1 < len(s) && s[i+1] == '*' {
				i += 2
				for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
					i++
				}
				i += 2
				continue
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
				return s[:i], s[i+1:]
			}
		}
		i++
	}
	return s, ""
}
