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
	s := &stmtScanner{ctx: ctx, onStmt: onStmt}
	for {
		n, rerr := r.Read(chunk)
		for i := range n {
			if err := s.feed(chunk[i]); err != nil {
				return s.applied, err
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return s.applied, rerr
		}
	}
	// A `-` or `/` pending at EOF was a literal trailing byte, not a
	// comment opener — write it before the final flush (mirrors the
	// byte-by-byte path, which wrote it once Peek returned empty).
	if s.dashSeen {
		s.buf.WriteByte('-')
	}
	if s.slashSeen {
		s.buf.WriteByte('/')
	}
	// Trailing statement without a closing `;` (rare but tolerated).
	if err := s.flush(); err != nil {
		return s.applied, err
	}
	return s.applied, nil
}

// stmtScanner carries the SQL-splitting state machine across the 1MB
// read chunks so the two-character `--` / `/*` / `*/` lookaheads and
// quote/comment context survive chunk boundaries.
type stmtScanner struct {
	ctx    context.Context
	onStmt func(stmt string) error
	buf    bytes.Buffer

	inSingle, inDouble, inBacktick bool
	inLineComment, inBlockComment  bool
	starSeen, dashSeen, slashSeen  bool
	applied                        uint64
}

func (s *stmtScanner) flush() error {
	stmt := s.buf.String()
	s.buf.Reset()
	if isBlank(stmt) {
		return nil
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := s.onStmt(stmt); err != nil {
		return fmt.Errorf("apply dump stmt #%d: %w", s.applied, err)
	}
	s.applied++
	return nil
}

// feed advances the scanner by one byte. Returns the error from flush
// only when a `;` terminator (or trailing-statement) callback fails.
func (s *stmtScanner) feed(b byte) error {
	if s.feedInContext(b) {
		return nil
	}
	// Top-level (not in a quote or comment). Resolve a pending
	// `-` / `/` from the previous byte first: it either forms a
	// comment opener or gets written verbatim before we handle b.
	if s.dashSeen {
		s.dashSeen = false
		if b == '-' {
			s.inLineComment = true
			return nil
		}
		s.buf.WriteByte('-')
	} else if s.slashSeen {
		s.slashSeen = false
		if b == '*' {
			s.inBlockComment = true
			return nil
		}
		s.buf.WriteByte('/')
	}
	switch b {
	case '-':
		s.dashSeen = true
	case '/':
		s.slashSeen = true
	case '\'':
		s.inSingle = true
		s.buf.WriteByte(b)
	case '"':
		s.inDouble = true
		s.buf.WriteByte(b)
	case '`':
		s.inBacktick = true
		s.buf.WriteByte(b)
	case ';':
		if err := s.flush(); err != nil {
			return err
		}
	default:
		s.buf.WriteByte(b)
	}
	return nil
}

// feedInContext handles a byte while inside a comment or quoted string,
// reporting true when b was consumed in such a context (so the caller
// skips top-level handling) and false when at top level.
func (s *stmtScanner) feedInContext(b byte) bool {
	switch {
	case s.inLineComment:
		if b == '\n' {
			s.inLineComment = false
		}
		return true
	case s.inBlockComment:
		if s.starSeen && b == '/' {
			s.inBlockComment = false
			s.starSeen = false
			return true
		}
		s.starSeen = b == '*'
		return true
	case s.inSingle:
		s.buf.WriteByte(b)
		if b == '\'' {
			s.inSingle = false
		}
		return true
	case s.inDouble:
		s.buf.WriteByte(b)
		if b == '"' {
			s.inDouble = false
		}
		return true
	case s.inBacktick:
		s.buf.WriteByte(b)
		if b == '`' {
			s.inBacktick = false
		}
		return true
	}
	return false
}

func isBlank(s string) bool {
	for _, c := range s {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}
