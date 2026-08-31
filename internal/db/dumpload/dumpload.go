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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/ident"
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
// That fall-through is only safe while the failed attempt wrote NOTHING
// — the usual case (no container, `mysql` missing inside it, bad
// credentials). An attempt that died MID-DUMP has already applied part
// of the file, and replaying the same dump on top of it produced "Table
// 'x' already exists" or, worse, a target whose schema was complete but
// whose skipped INSERTs left the framework `migrations` table empty, so
// the following `migrate` replayed every migration and died on a
// duplicate column (issue #28). So the target is probed for new objects
// after a failed attempt: if it gained any, the error is returned
// instead of falling through, and the next prepare cold-builds the
// database from scratch.
//
// Compression (gzip/zstd/bzip2/xz) is auto-detected from the file's
// magic bytes — extension is not consulted.
func LoadMySQL(ctx context.Context, db *sql.DB, conn *config.MysqlConn, targetDB, dumpPath string) (LoadStrategy, error) {
	if err := ensureMySQLDB(ctx, db, targetDB); err != nil {
		return "", err
	}
	// Only the fast paths can leave a half-applied dump behind, and they
	// need `conn`; the wire-only path (conn == nil) skips the probe.
	countObjects := func() (int, bool) { return mysqlObjectCount(ctx, db, targetDB) }
	before, counted := 0, false
	if conn != nil {
		before, counted = countObjects()
	}
	if ok, err := runFastPathMySQL(ctx, conn, targetDB, dumpPath, tryDockerExecMySQL); ok {
		return StrategyDockerExec, nil
	} else if err != nil {
		if perr := partialLoadError("docker exec", dumpPath, targetDB, err, before, counted, countObjects); perr != nil {
			return "", perr
		}
	}
	if ok, err := runFastPathMySQL(ctx, conn, targetDB, dumpPath, tryNativeCLIMySQL); ok {
		return StrategyNativeCLI, nil
	} else if err != nil {
		if perr := partialLoadError("native CLI", dumpPath, targetDB, err, before, counted, countObjects); perr != nil {
			return "", perr
		}
	}
	return StrategyWire, loadMySQLViaWire(ctx, db, targetDB, dumpPath)
}

// partialLoadError decides what a failed fast-path attempt means. It
// returns nil when the attempt provably wrote nothing (the target holds
// the same object count as before it ran, or the count is unknown) — the
// caller then logs a warning and falls through to the next strategy, the
// long-standing "a misconfigured CLI can never block a cold build"
// behaviour. It returns an error when the target GAINED objects: the
// dump was applied in part, replaying it would collide or silently skip
// the aborted section's rows, and the honest move is to fail so the next
// prepare rebuilds the database from scratch (issue #28).
//
// ponytail: object count, not content. An attempt that died after
// INSERTing rows into existing tables without creating any is
// indistinguishable from one that wrote nothing, so a data-only dump can
// still be replayed on top of itself. Compare a row/checksum watermark
// per table if that ever bites.
func partialLoadError(
	strategy, dumpPath, targetDB string,
	attemptErr error,
	before int,
	counted bool,
	countObjects func() (int, bool),
) error {
	if counted {
		if after, ok := countObjects(); ok && after != before {
			return fmt.Errorf(
				"dump-load fast path (%s) aborted after applying part of %s: target %s gained %d object(s) "+
					"and replaying the dump on top would corrupt it; not falling through: %w",
				strategy, dumpPath, targetDB, after-before, attemptErr)
		}
	}
	slog.Warn("dump-load fast path failed; falling through",
		"strategy", strategy, "target", targetDB, "error", attemptErr)
	return nil
}

// mysqlObjectCount counts tables + views in `targetDB`. The bool is
// false when the count could not be taken (nil handle, query error), in
// which case callers keep the old fall-through behaviour rather than
// blocking a cold build on a probe failure.
func mysqlObjectCount(ctx context.Context, db *sql.DB, targetDB string) (int, bool) {
	if db == nil {
		return 0, false
	}
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ?",
		targetDB).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// postgresObjectCount mirrors mysqlObjectCount for a *sql.DB already
// scoped to the target database (pg has no USE), counting every
// non-system schema so a dump that creates its own schemas is covered.
func postgresObjectCount(ctx context.Context, db *sql.DB) (int, bool) {
	if db == nil {
		return 0, false
	}
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables "+
			"WHERE table_schema NOT IN ('pg_catalog', 'information_schema')").Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

// LoadPostgres mirrors LoadMySQL for pgx-backed databases. The `db`
// arg must be a connection scoped to `targetDB` (pg has no USE); the
// wire-protocol fallback uses it directly. The fast paths consult
// `conn` and reconnect via the CLI.
func LoadPostgres(ctx context.Context, db *sql.DB, conn *config.PostgresConn, targetDB, dumpPath string) (LoadStrategy, error) {
	// See LoadMySQL: probe only when a fast path can actually run.
	countObjects := func() (int, bool) { return postgresObjectCount(ctx, db) }
	before, counted := 0, false
	if conn != nil {
		before, counted = countObjects()
	}
	if ok, err := runFastPathPostgres(ctx, conn, targetDB, dumpPath, tryDockerExecPostgres); ok {
		return StrategyDockerExec, nil
	} else if err != nil {
		if perr := partialLoadError("docker exec", dumpPath, targetDB, err, before, counted, countObjects); perr != nil {
			return "", perr
		}
	}
	if ok, err := runFastPathPostgres(ctx, conn, targetDB, dumpPath, tryNativeCLIPostgres); ok {
		return StrategyNativeCLI, nil
	} else if err != nil {
		if perr := partialLoadError("native CLI", dumpPath, targetDB, err, before, counted, countObjects); perr != nil {
			return "", perr
		}
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
	_, err = streamStatements(ctx, f, retryingExec(ctx, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	}))
	return err
}

// ensureMySQLDB creates `targetDB` when it is missing, so a dump load
// never dies on `USE …: Error 1049 Unknown database`. A prepare that
// fails between cold-build's DROP DATABASE and the dump load leaves the
// source gone while SQLite still holds a snapshot row for it; before
// this, every retry failed on the missing database and the only way out
// was a manual `snapshots_drop` (#24). Idempotent, and the DDL matches
// mysql.Driver.EnsureDB so a DB created here is not charset-divergent.
func ensureMySQLDB(ctx context.Context, db *sql.DB, targetDB string) error {
	if db == nil {
		return nil
	}
	qname, err := ident.QuoteMySQL(targetDB)
	if err != nil {
		return err
	}
	//nolint:gosec // qname is validated/quoted via ident.QuoteMySQL; no user values concatenated
	stmt := "CREATE DATABASE IF NOT EXISTS " + qname +
		" DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("CREATE DATABASE %s: %w", qname, err)
	}
	return nil
}

// stmtLockRetries / stmtLockBackoff bound the per-statement retry of an
// InnoDB lock conflict. Contention comes from sibling worktrees loading
// dumps into their own databases at the same time; the loser is told to
// restart its transaction and usually wins within a few hundred ms.
const stmtLockRetries = 4

var stmtLockBackoff = []time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}

// retryingExec wraps a per-statement exec so a deadlock (1213) or lock-
// wait timeout (1205) retries instead of aborting the whole dump load
// — both are explicitly restartable, and losing a 10-minute load to one
// of them left the worktree unpreparable (#24).
//
// Retrying is only sound under autocommit, where the rolled-back
// transaction is the single failed statement. Dumps that open an
// explicit transaction (`BEGIN` / `START TRANSACTION`) get no retry:
// InnoDB rolled back everything since the BEGIN, so re-running just the
// last statement would silently commit a partial transaction.
func retryingExec(ctx context.Context, exec func(stmt string) error) func(stmt string) error {
	inTx := false
	return func(stmt string) error {
		for attempt := 0; ; attempt++ {
			err := exec(stmt)
			if err == nil {
				inTx = txStateAfter(inTx, stmt)
				return nil
			}
			if inTx || attempt >= stmtLockRetries || !isLockContention(err) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(stmtLockBackoff[min(attempt, len(stmtLockBackoff)-1)]):
			}
		}
	}
}

// txStateAfter reports whether an explicit transaction is open after
// `stmt` ran. Only the statements mysqldump emits are recognised;
// anything else leaves the state unchanged.
func txStateAfter(inTx bool, stmt string) bool {
	s := strings.ToUpper(strings.TrimLeft(stmt, " \t\r\n"))
	switch {
	case strings.HasPrefix(s, "BEGIN"), strings.HasPrefix(s, "START TRANSACTION"):
		return true
	case strings.HasPrefix(s, "COMMIT"), strings.HasPrefix(s, "ROLLBACK"):
		return false
	}
	return inTx
}

// isLockContention reports whether err is an InnoDB deadlock (1213) or
// lock-wait timeout (1205) — the two errors MySQL itself documents as
// "try restarting transaction".
func isLockContention(err error) bool {
	var me *gomysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == 1213 || me.Number == 1205
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
	s := &stmtScanner{onStmt: onStmt}
	for {
		if err := ctx.Err(); err != nil {
			return s.applied, err
		}
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
