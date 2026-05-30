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
	"log/slog"

	"github.com/stubbedev/treeman/internal/config"
)

// LoadMySQL applies `dumpPath` to `targetDB` using the fastest
// available path. Strategy is selected in order:
//
//  1. `<container-engine> exec -i CID mysql …` — when `conn` is non-nil
//     and `conn.ContainerRef` resolves to a running container. Fastest
//     because the CLI talks to mysqld over an in-container unix socket.
//  2. Native `mysql` CLI on PATH piping the (decompressed) dump. Skips
//     the per-statement round-trips that the wire loader pays.
//  3. Wire-protocol streaming over the existing *sql.DB connection.
//     Always available, and the only path used when `conn` is nil.
//
// `conn` is optional: pass it so the fast paths can read host/port/
// user/password; pass nil to force the wire path (used by branch-
// scoped loadDump callers that only carry a *sql.DB).
//
// Returns the strategy that actually ran so callers can record it as
// an event. A fast-path FAILURE (exec ran but exited non-zero) is
// downgraded to a warning log + fall-through to the next strategy, so
// a misconfigured CLI on a dev box can never block a cold build.
//
// Compression (gzip/zstd/bzip2/xz) is auto-detected from the file's
// magic bytes — extension is not consulted.
func LoadMySQL(ctx context.Context, db *sql.DB, conn *config.MysqlConn, targetDB, dumpPath string) (LoadStrategy, error) {
	if ok, err := runFastPathMySQL(ctx, conn, targetDB, dumpPath, tryDockerExecMySQL); ok {
		return StrategyDockerExec, nil
	} else if err != nil {
		slog.Warn("dump-load fast path (docker exec) failed; falling through", "error", err)
	}
	if ok, err := runFastPathMySQL(ctx, conn, targetDB, dumpPath, tryNativeCLIMySQL); ok {
		return StrategyNativeCLI, nil
	} else if err != nil {
		slog.Warn("dump-load fast path (native CLI) failed; falling through", "error", err)
	}
	return StrategyWire, loadMySQLViaWire(ctx, db, targetDB, dumpPath)
}

// LoadPostgres mirrors LoadMySQL for pgx-backed databases. The `db`
// arg must be a connection scoped to `targetDB` (pg has no USE); the
// wire-protocol fallback uses it directly. The fast paths consult
// `conn` and reconnect via the CLI.
func LoadPostgres(ctx context.Context, db *sql.DB, conn *config.PostgresConn, targetDB, dumpPath string) (LoadStrategy, error) {
	if ok, err := runFastPathPostgres(ctx, conn, targetDB, dumpPath, tryDockerExecPostgres); ok {
		return StrategyDockerExec, nil
	} else if err != nil {
		slog.Warn("dump-load fast path (docker exec) failed; falling through", "error", err)
	}
	if ok, err := runFastPathPostgres(ctx, conn, targetDB, dumpPath, tryNativeCLIPostgres); ok {
		return StrategyNativeCLI, nil
	} else if err != nil {
		slog.Warn("dump-load fast path (native CLI) failed; falling through", "error", err)
	}
	return StrategyWire, loadPostgresViaWire(ctx, db, dumpPath)
}

// runFastPathMySQL opens the dump (with compression sniffing), hands
// the decompressed reader to `attempt`, and forwards the (ok, err)
// triple back to the dispatcher. Centralises file open + close so
// each tryXxx helper stays focused on the exec mechanics.
func runFastPathMySQL(ctx context.Context, conn *config.MysqlConn, targetDB, dumpPath string,
	attempt func(context.Context, *config.MysqlConn, string, io.Reader) (bool, error),
) (bool, error) {
	if conn == nil {
		return false, nil
	}
	f, _, err := OpenDump(dumpPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	return attempt(ctx, conn, targetDB, f)
}

// runFastPathPostgres mirrors runFastPathMySQL for the psql helpers.
func runFastPathPostgres(ctx context.Context, conn *config.PostgresConn, targetDB, dumpPath string,
	attempt func(context.Context, *config.PostgresConn, string, io.Reader) (bool, error),
) (bool, error) {
	if conn == nil {
		return false, nil
	}
	f, _, err := OpenDump(dumpPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	return attempt(ctx, conn, targetDB, f)
}

// loadMySQLViaWire is the original statement-streaming loader. Uses
// streamStatements internally so a 10GB dump is bounded by the largest
// single statement size (typically ~64MB for mysqldump extended-
// inserts), not the file size.
func loadMySQLViaWire(ctx context.Context, db *sql.DB, targetDB, dumpPath string) error {
	f, _, err := OpenDump(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("USE `%s`", targetDB)); err != nil {
		return fmt.Errorf("USE `%s`: %w", targetDB, err)
	}
	for _, s := range []string{
		"SET SESSION foreign_key_checks=0",
		"SET SESSION unique_checks=0",
		"SET SESSION sql_log_bin=0",
	} {
		_, _ = conn.ExecContext(ctx, s) // best-effort; some flags need SUPER
	}
	_, err = streamStatements(ctx, f, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
	return err
}

// loadPostgresViaWire is the original statement-streaming loader.
// Caller must have opened db against the target database (pg has no
// USE), which it always does (preparePostgres uses OpenScoped).
func loadPostgresViaWire(ctx context.Context, db *sql.DB, dumpPath string) error {
	f, _, err := OpenDump(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = streamStatements(ctx, f, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
	return err
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
