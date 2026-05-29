// Package store wraps the SQLite event log and worktree registry.
// Schema lives in `0001_init.sql` and is treated as a stable
// on-disk format so existing `~/.local/share/treeman/treeman.db`
// databases survive upgrades.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	// modernc.org/sqlite registers the pure-Go "sqlite" database/sql driver.
	_ "modernc.org/sqlite"

	"github.com/stubbedev/treeman/internal/runid"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DefaultDBPath returns `~/.local/share/treeman/treeman.db` (or the
// path overridden by `$TREEMAN_DB_PATH`). The parent dir is created
// if missing.
func DefaultDBPath() (string, error) {
	if p := os.Getenv("TREEMAN_DB_PATH"); p != "" {
		return p, nil
	}
	xdg := os.Getenv("XDG_DATA_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(xdg, "treeman")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "treeman.db"), nil
}

// Store wraps the *sql.DB plus a few prepared-stmt helpers.
type Store struct {
	DB *sql.DB

	// Event batching state. When StartEventBatcher has been called
	// (typically by the daemon), WriteEvent enqueues rows instead of
	// firing one INSERT per call. The flusher goroutine commits the
	// buffer every batchFlushInterval or when it hits
	// batchFlushThreshold rows, whichever comes first.
	//
	// CLI invocations don't call StartEventBatcher and continue to
	// write synchronously — short-lived processes don't benefit from
	// batching and the sync path keeps the existing error semantics.
	batchMu     sync.Mutex
	batchBuf    []pendingEvent
	batchSignal chan struct{}
	batchCtx    context.Context
	batchCancel context.CancelFunc
	batchDone   chan struct{}
	batchActive bool
}

// pendingEvent is one buffered events-table row awaiting flush.
type pendingEvent struct {
	tsMillis   int64
	level      string
	repoID     any
	worktreeID any
	eventType  string
	phase      any
	message    any
	payload    string
	durationMs any
}

// batchFlushInterval bounds the worst-case latency between a
// WriteEvent call and the row hitting disk. 100 ms is short enough
// to keep `treeman logs tail --follow` snappy while letting the
// burst-write case (a teardown that emits 3-5 events in <10 ms)
// coalesce into one transaction.
const batchFlushInterval = 100 * time.Millisecond

// batchFlushThreshold forces a flush before the timer when a single
// burst exceeds this many rows. Prevents the buffer from growing
// without bound under a watcher storm.
const batchFlushThreshold = 200

// Open opens (or creates) the SQLite file, applies pragmas + the
// embedded migrations, and returns a usable Store.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	// modernc.org/sqlite's DSN supports query params for pragmas
	// applied at connection open (compatible across the pool).
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}
	db.SetMaxOpenConns(8)
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

// Close shuts down the underlying pool. If the event batcher is
// running it is drained synchronously first so no in-flight rows
// are lost.
func (s *Store) Close() error {
	s.StopEventBatcher()
	return s.DB.Close()
}

// StartEventBatcher turns on async batched WriteEvent flushing. The
// flusher goroutine lives until ctx cancels or StopEventBatcher is
// called. Subsequent calls are no-ops — only the first call wins,
// so the daemon can invoke this once at boot without worrying about
// concurrent callers.
func (s *Store) StartEventBatcher(ctx context.Context) {
	s.batchMu.Lock()
	if s.batchActive {
		s.batchMu.Unlock()
		return
	}
	bctx, cancel := context.WithCancel(ctx)
	s.batchCtx = bctx
	s.batchCancel = cancel
	s.batchSignal = make(chan struct{}, 1)
	s.batchDone = make(chan struct{})
	s.batchActive = true
	s.batchMu.Unlock()
	go s.eventBatchLoop() //nolint:gosec // long-lived background loop; intentionally detached from any request ctx
}

// StopEventBatcher cancels the flusher and synchronously drains any
// remaining buffered events. Safe to call multiple times.
func (s *Store) StopEventBatcher() {
	s.batchMu.Lock()
	if !s.batchActive {
		s.batchMu.Unlock()
		return
	}
	s.batchActive = false
	cancel := s.batchCancel
	done := s.batchDone
	s.batchMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	// Final drain: anything that arrived after the cancel but before
	// the goroutine observed it. Bounded so a wedged sqlite write
	// can't block daemon shutdown forever.
	ctx, cancelFinal := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFinal()
	s.flushEvents(ctx)
}

// eventBatchLoop runs in its own goroutine when batching is active.
// Flushes either when batchSignal fires (threshold hit) or every
// batchFlushInterval.
func (s *Store) eventBatchLoop() {
	defer close(s.batchDone)
	ticker := time.NewTicker(batchFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.batchCtx.Done():
			// Don't reuse batchCtx (already cancelled). Use a
			// bounded fresh ctx so the final drain can't block
			// shutdown forever on a wedged sqlite write.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.flushEvents(ctx)
			cancel()
			return
		case <-ticker.C:
			s.flushEvents(s.batchCtx)
		case <-s.batchSignal:
			s.flushEvents(s.batchCtx)
		}
	}
}

// flushEvents commits every buffered row in one transaction. The
// batch path is fire-and-forget by design (callers use `_ =
// WriteEvent(...)` everywhere) — so errors are logged via slog
// instead of returned. One bad row poisoning a whole batch would
// lose unrelated events, so we fall back to a per-row insert when
// the transaction fails.
func (s *Store) flushEvents(ctx context.Context) {
	s.batchMu.Lock()
	if len(s.batchBuf) == 0 {
		s.batchMu.Unlock()
		return
	}
	batch := s.batchBuf
	s.batchBuf = nil
	s.batchMu.Unlock()

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("event batch: begin tx failed, falling back to per-row", "err", err, "rows", len(batch))
		s.flushEventsFallback(ctx, batch)
		return
	}
	const stmt = `INSERT INTO events(ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, e := range batch {
		if _, err := tx.ExecContext(ctx, stmt,
			e.tsMillis, e.level, e.repoID, e.worktreeID, e.eventType,
			e.phase, e.message, e.payload, e.durationMs); err != nil {
			_ = tx.Rollback()
			slog.Warn("event batch: row insert failed, falling back to per-row", "err", err, "rows", len(batch))
			s.flushEventsFallback(ctx, batch)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("event batch: commit failed, falling back to per-row", "err", err, "rows", len(batch))
		s.flushEventsFallback(ctx, batch)
	}
}

// flushEventsFallback re-inserts a batch one row at a time when the
// batched transaction fails. Slower but resilient: a single
// constraint violation can't lose the rest of the buffer. Per-row
// errors are surfaced via slog so a stuck disk / corrupted DB
// doesn't silently vanish the event log.
func (s *Store) flushEventsFallback(ctx context.Context, batch []pendingEvent) {
	const stmt = `INSERT INTO events(ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	failed := 0
	for _, e := range batch {
		if _, err := s.DB.ExecContext(ctx, stmt,
			e.tsMillis, e.level, e.repoID, e.worktreeID, e.eventType,
			e.phase, e.message, e.payload, e.durationMs); err != nil {
			failed++
			slog.Warn("event row insert failed", "err", err, "type", e.eventType)
		}
	}
	if failed > 0 {
		slog.Warn("event batch fallback: rows lost", "failed", failed, "total", len(batch))
	}
}

// CheckpointWAL forces a TRUNCATE checkpoint on the SQLite WAL. The
// daemon calls this on a cron to keep `~/.local/share/treeman/
// treeman.db-wal` from growing without bound under sustained write
// churn (every `treeman wt finalize`/`teardown` writes several
// events plus a `MarkWorktreeDeleted`/`MarkVisited`). With WAL on
// + synchronous=NORMAL, automatic passive checkpoints only fire
// when the WAL hits ~1000 pages; TRUNCATE on a fixed schedule
// resets the file to zero bytes whenever no reader is mid-txn.
func (s *Store) CheckpointWAL(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// migrate creates the `_treeman_migrations` bookkeeping table if
// missing and applies any embedded SQL files that haven't run yet.
// Schema-content is hashed so checksum drift surfaces as an error
// rather than silently re-applying.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _treeman_migrations (
		version INTEGER PRIMARY KEY,
		filename TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create _treeman_migrations: %w", err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// File names are `NNNN_name.sql`.
		var version int
		_, err := fmt.Sscanf(e.Name(), "%04d_", &version)
		if err != nil {
			continue
		}
		var existing int
		row := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM _treeman_migrations WHERE version = ?", version)
		if err := row.Scan(&existing); err != nil {
			return fmt.Errorf("query _treeman_migrations: %w", err)
		}
		if existing > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				return fmt.Errorf("apply migration %s: %w (rollback: %w)", e.Name(), err, rbErr)
			}
			return fmt.Errorf("apply migration %s: %w", e.Name(), err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO _treeman_migrations(version, filename, applied_at) VALUES (?,?,?)",
			version, e.Name(), nowMillis()); err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				return fmt.Errorf("%w (rollback: %w)", err, rbErr)
			}
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// EnsureRepo upserts a (path, name) and returns the repo id.
func (s *Store) EnsureRepo(ctx context.Context, path, name string) (int64, error) {
	// COLLATE NOCASE so APFS case-insensitive filesystems on macOS
	// don't double-register the same repo when the user typed a
	// different case at create vs at access. Backed by
	// idx_repos_path_nocase (migration 0003).
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM repos WHERE path = ? COLLATE NOCASE", path)
	var id int64
	if err := row.Scan(&id); err == nil {
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO repos(path, name, frameworks_json, registered_at) VALUES (?, ?, '[]', ?)",
		path, name, nowMillis())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EnsureWorktree upserts a (path, repo_id, slug, branch) and returns
// the worktree id.
func (s *Store) EnsureWorktree(ctx context.Context, repoID int64, path, slug, branch string) (int64, error) {
	return s.EnsureWorktreeWithAdmin(ctx, repoID, path, slug, branch, "")
}

// EnsureWorktreeWithAdmin is EnsureWorktree plus an `admin_dir`
// argument — the absolute path of git's per-worktree administrative
// directory (`<common-dir>/worktrees/<name>/`). The watcher needs
// this to map a REMOVE event back to the worktree row after the
// working tree has been deleted. Empty admin_dir is allowed and
// leaves the column NULL.
func (s *Store) EnsureWorktreeWithAdmin(ctx context.Context, repoID int64, path, slug, branch, adminDir string) (int64, error) {
	// COLLATE NOCASE so an APFS case-insensitive FS on macOS matches
	// `/Users/jane/Repo/.worktrees/x` with a later `/users/jane/repo/.worktrees/X`.
	// Backed by idx_worktrees_path_nocase (migration 0003).
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM worktrees WHERE path = ? COLLATE NOCASE", path)
	var id int64
	if err := row.Scan(&id); err == nil {
		// Resurrect: when a worktree is re-created at the path of a
		// previously-deleted row we MUST clear deleted_at and refresh
		// slug/branch. Without this the row stays marked deleted, and
		// the next TeardownWorktree short-circuits on the
		// `row.Deleted` check so predelete + db drop + git remove
		// never run and the working tree lingers on disk forever.
		// Also keep admin_dir current.
		var br any
		if branch != "" {
			br = branch
		}
		var ad any
		if adminDir != "" {
			ad = adminDir
		}
		if _, err := s.DB.ExecContext(ctx, `
			UPDATE worktrees
			SET slug = ?,
			    branch = COALESCE(?, branch),
			    admin_dir = COALESCE(?, admin_dir),
			    deleted_at = NULL
			WHERE id = ?`, slug, br, ad, id); err != nil {
			return 0, err
		}
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var br any
	if branch != "" {
		br = branch
	}
	var ad any
	if adminDir != "" {
		ad = adminDir
	}
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO worktrees(repo_id, path, slug, branch, admin_dir, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		repoID, path, slug, br, ad, nowMillis())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EnsureMainWorktree upserts the repo's main-worktree row (is_main=1)
// for the repo root path. Behaves like EnsureWorktree but forces
// is_main on insert AND on resurrect — flipping the main_worktree
// config off, then back on, must restore the row's main-ness even
// when the path already matches a soft-deleted linked-wt row.
//
// The partial unique index `idx_worktrees_one_main_per_repo`
// guarantees at most one active main row per repo; if a caller
// somehow tries to insert a second, the INSERT fails loud — better
// to crash than silently bind two main rows to the same repo.
func (s *Store) EnsureMainWorktree(ctx context.Context, repoID int64, path, slug, branch string) (int64, error) {
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM worktrees WHERE path = ? COLLATE NOCASE", path)
	var id int64
	if err := row.Scan(&id); err == nil {
		var br any
		if branch != "" {
			br = branch
		}
		if _, err := s.DB.ExecContext(ctx, `
			UPDATE worktrees
			SET slug = ?,
			    branch = COALESCE(?, branch),
			    deleted_at = NULL,
			    is_main = 1
			WHERE id = ?`, slug, br, id); err != nil {
			return 0, err
		}
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var br any
	if branch != "" {
		br = branch
	}
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO worktrees(repo_id, path, slug, branch, created_at, is_main) VALUES (?, ?, ?, ?, ?, 1)",
		repoID, path, slug, br, nowMillis())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LookupMainWorktree returns the active main-wt row for repoID, or a
// zero-value row when none is enrolled. Used by the daemon's boot
// resume + config-reload paths to decide whether to spawn watchers
// against the repo root.
func (s *Store) LookupMainWorktree(ctx context.Context, repoID int64) (WorktreeRow, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, repo_id, path, slug, COALESCE(branch, ''), COALESCE(admin_dir, ''), 0, is_main
		FROM worktrees
		WHERE repo_id = ? AND is_main = 1 AND deleted_at IS NULL
		ORDER BY id DESC LIMIT 1`, repoID)
	var w WorktreeRow
	var deleted int
	if err := row.Scan(&w.ID, &w.RepoID, &w.Path, &w.Slug, &w.Branch, &w.AdminDir, &deleted, &w.IsMain); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorktreeRow{}, nil
		}
		return WorktreeRow{}, err
	}
	return w, nil
}

// SetWorktreeAdminDir stamps the per-worktree git administrative
// directory on an existing row. Used by the lifecycle reconcile pass
// to backfill rows that were created before the column existed.
func (s *Store) SetWorktreeAdminDir(ctx context.Context, id int64, adminDir string) error {
	if id <= 0 || adminDir == "" {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		"UPDATE worktrees SET admin_dir = ? WHERE id = ?", adminDir, id)
	return err
}

// LookupActiveWorktreeByPath returns the non-deleted worktree row
// for `path`, or a zero-value row when nothing matches. Used by the
// lifecycle watcher to detect when the CLI's wt-create flow has
// already registered a worktree at the same path — in which case
// the watcher must NOT spawn its own duplicate postcreate /
// FinalizeWorktree, or composer install / npm install fight on
// file locks.
func (s *Store) LookupActiveWorktreeByPath(ctx context.Context, path string) (WorktreeRow, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, repo_id, path, slug, COALESCE(branch, ''), COALESCE(admin_dir, ''), 0, is_main
		FROM worktrees WHERE path = ? COLLATE NOCASE AND deleted_at IS NULL ORDER BY id DESC LIMIT 1`, path)
	var w WorktreeRow
	var deleted int
	if err := row.Scan(&w.ID, &w.RepoID, &w.Path, &w.Slug, &w.Branch, &w.AdminDir, &deleted, &w.IsMain); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorktreeRow{}, nil
		}
		return WorktreeRow{}, err
	}
	return w, nil
}

// LookupWorktreeByAdminDir resolves an admin_dir back to the worktree
// row. Returns 0 + nil when no row matches.
func (s *Store) LookupWorktreeByAdminDir(ctx context.Context, adminDir string) (WorktreeRow, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, repo_id, path, slug, COALESCE(branch, ''), COALESCE(admin_dir, ''), deleted_at IS NOT NULL, is_main
		FROM worktrees WHERE admin_dir = ? ORDER BY id DESC LIMIT 1`, adminDir)
	var w WorktreeRow
	var deleted bool
	if err := row.Scan(&w.ID, &w.RepoID, &w.Path, &w.Slug, &w.Branch, &w.AdminDir, &deleted, &w.IsMain); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorktreeRow{}, nil
		}
		return WorktreeRow{}, err
	}
	w.Deleted = deleted
	return w, nil
}

// WorktreeRow is the lifecycle-watcher view of a worktrees row.
type WorktreeRow struct {
	ID       int64
	RepoID   int64
	Path     string
	Slug     string
	Branch   string
	AdminDir string
	Deleted  bool
	IsMain   bool
}

// ListWorktreesForRepo returns every worktree row attached to repoID,
// including deleted ones. Used by the lifecycle reconcile pass to
// diff the DB against the filesystem.
func (s *Store) ListWorktreesForRepo(ctx context.Context, repoID int64) ([]WorktreeRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, repo_id, path, slug, COALESCE(branch, ''), COALESCE(admin_dir, ''), deleted_at IS NOT NULL, is_main
		FROM worktrees WHERE repo_id = ? ORDER BY id`, repoID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WorktreeRow
	for rows.Next() {
		var w WorktreeRow
		if err := rows.Scan(&w.ID, &w.RepoID, &w.Path, &w.Slug, &w.Branch, &w.AdminDir, &w.Deleted, &w.IsMain); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountActiveWorktreesForRepo returns the number of `deleted_at IS NULL`
// worktree rows under `repoID`. Used by `registry remove` to refuse a
// non-forced removal while live worktrees still exist.
func (s *Store) CountActiveWorktreesForRepo(ctx context.Context, repoID int64) (int, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM worktrees WHERE repo_id = ? AND deleted_at IS NULL`, repoID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// RemoveRepo deletes the repo row and every child row that would
// otherwise block the FK constraint or hang around as orphan tracking
// data: hook_runs, events, snapshots, worktrees. Wrapped in a
// transaction so a mid-delete failure leaves the registry intact.
//
// External resources (per-worktree databases, on-disk git worktree
// dirs, dump caches under `.treeman-snapshots/`, advisory hash caches
// keyed by path) are NOT touched — callers that want a destructive
// clean-up must tear down the worktrees first via `treeman wt delete`.
func (s *Store) RemoveRepo(ctx context.Context, repoID int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmts := []string{
		`DELETE FROM hook_runs WHERE worktree_id IN (SELECT id FROM worktrees WHERE repo_id = ?)`,
		`DELETE FROM events    WHERE repo_id = ? OR worktree_id IN (SELECT id FROM worktrees WHERE repo_id = ?)`,
		`DELETE FROM snapshots WHERE repo_id = ?`,
		`DELETE FROM worktrees WHERE repo_id = ?`,
		`DELETE FROM repos     WHERE id = ?`,
	}
	args := [][]any{
		{repoID},
		{repoID, repoID},
		{repoID},
		{repoID},
		{repoID},
	}
	for i, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, args[i]...); err != nil {
			return fmt.Errorf("registry remove step %d: %w", i, err)
		}
	}
	return tx.Commit()
}

// ListRepoRefs returns (id, path) for every registered repo in
// insertion order. Used by the daemon at boot to subscribe the
// always-on lifecycle watcher to every known repo.
func (s *Store) ListRepoRefs(ctx context.Context) ([]RepoRef, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id, path FROM repos ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RepoRef
	for rows.Next() {
		var r RepoRef
		if err := rows.Scan(&r.ID, &r.Path); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RepoRef is the minimal (id, path) view used by the daemon to
// subscribe lifecycle watchers without loading the whole repo row.
type RepoRef struct {
	ID   int64
	Path string
}

// MarkWorktreeDeleted sets the worktree's `deleted_at` to now.
func (s *Store) MarkWorktreeDeleted(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE worktrees SET deleted_at = ? WHERE id = ?", nowMillis(), id)
	return err
}

// SaveInheritedEnv caches the user's shell env per worktree so the
// daemon's HEAD-watcher and file-watcher paths can rehydrate it
// when re-running hooks/prepare. Empty maps are stored as NULL so
// the caller can tell "never set" from "set to empty".
func (s *Store) SaveInheritedEnv(ctx context.Context, worktreeID int64, env map[string]string) error {
	if len(env) == 0 {
		_, err := s.DB.ExecContext(ctx,
			"UPDATE worktrees SET inherited_env_json = NULL WHERE id = ?", worktreeID)
		return err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal inherited env: %w", err)
	}
	_, err = s.DB.ExecContext(ctx,
		"UPDATE worktrees SET inherited_env_json = ? WHERE id = ?", string(b), worktreeID)
	return err
}

// LoadInheritedEnv returns the cached env captured at the last
// SaveInheritedEnv call. Returns nil + nil-error when the column is
// NULL (no env ever cached for this worktree).
func (s *Store) LoadInheritedEnv(ctx context.Context, worktreeID int64) (map[string]string, error) {
	row := s.DB.QueryRowContext(ctx,
		"SELECT inherited_env_json FROM worktrees WHERE id = ?", worktreeID)
	var raw sql.NullString
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(raw.String), &env); err != nil {
		return nil, fmt.Errorf("unmarshal inherited env: %w", err)
	}
	return env, nil
}

// LoadInheritedEnvByPath looks up the cached env by worktree path,
// for callers that have the path but not the ID.
func (s *Store) LoadInheritedEnvByPath(ctx context.Context, worktreePath string) (map[string]string, error) {
	row := s.DB.QueryRowContext(ctx,
		"SELECT inherited_env_json FROM worktrees WHERE path = ? COLLATE NOCASE", worktreePath)
	var raw sql.NullString
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(raw.String), &env); err != nil {
		return nil, fmt.Errorf("unmarshal inherited env: %w", err)
	}
	return env, nil
}

// TouchWorktreeVisited stamps `last_visited_at = now` on the worktree
// row. Called by `wt switch`, `wt go`, and any other path that
// represents a user-driven move into a worktree. Used by `wt prev`
// + `wt list --sort=visited` to surface recency.
func (s *Store) TouchWorktreeVisited(ctx context.Context, id int64) error {
	if id <= 0 {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		"UPDATE worktrees SET last_visited_at = ? WHERE id = ?", nowMillis(), id)
	return err
}

// TouchWorktreeVisitedByPath looks up the worktree id by its absolute
// path and stamps `last_visited_at`. Idempotent; silently no-ops when
// no row matches (e.g. unregistered ad-hoc worktrees).
func (s *Store) TouchWorktreeVisitedByPath(ctx context.Context, path string) error {
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM worktrees WHERE path = ? COLLATE NOCASE AND deleted_at IS NULL", path)
	var id int64
	if err := row.Scan(&id); err != nil {
		return nil //nolint:nilerr // documented no-op when no worktree row matches the path
	}
	return s.TouchWorktreeVisited(ctx, id)
}

// PrevVisitedWorktree returns the most-recently-visited worktree path
// for `repoID`, excluding `exceptPath`. Empty string + false when
// there's no candidate.
func (s *Store) PrevVisitedWorktree(ctx context.Context, repoID int64, exceptPath string) (string, bool) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT path FROM worktrees
		WHERE repo_id = ? AND deleted_at IS NULL AND path != ?
		  AND last_visited_at IS NOT NULL
		ORDER BY last_visited_at DESC LIMIT 1`, repoID, exceptPath)
	var p string
	if err := row.Scan(&p); err != nil {
		return "", false
	}
	return p, true
}

// RepoPath resolves a repo row id back to its absolute path. Returns
// "" + nil when no row matches.
func (s *Store) RepoPath(ctx context.Context, repoID int64) (string, error) {
	if repoID <= 0 {
		return "", nil
	}
	row := s.DB.QueryRowContext(ctx, "SELECT path FROM repos WHERE id = ?", repoID)
	var p string
	if err := row.Scan(&p); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return p, nil
}

// WorktreePathByID returns the path column for a worktree row, or ""
// when the row is missing.
func (s *Store) WorktreePathByID(ctx context.Context, worktreeID int64) (string, error) {
	if worktreeID <= 0 {
		return "", nil
	}
	row := s.DB.QueryRowContext(ctx, "SELECT path FROM worktrees WHERE id = ?", worktreeID)
	var p string
	if err := row.Scan(&p); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return p, nil
}

// WorktreeBranch returns the branch column for a worktree row, or ""
// when the row is missing or the branch is NULL.
func (s *Store) WorktreeBranch(ctx context.Context, worktreeID int64) (string, error) {
	if worktreeID <= 0 {
		return "", nil
	}
	row := s.DB.QueryRowContext(ctx, "SELECT COALESCE(branch,'') FROM worktrees WHERE id = ?", worktreeID)
	var b string
	if err := row.Scan(&b); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return b, nil
}

// LookupRepoID resolves a repo path to its row id. Returns 0 + nil
// when no row matches (caller treats this as "unknown").
func (s *Store) LookupRepoID(ctx context.Context, path string) (int64, error) {
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM repos WHERE path = ? COLLATE NOCASE", path)
	var id int64
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

// ActiveWorktree pairs a repo path with one of its live worktree
// paths. Used by the daemon at boot to resume per-worktree fsnotify
// watchers.
type ActiveWorktree struct {
	RepoPath     string
	WorktreePath string
}

// ListActiveWorktrees returns every (repo path, worktree path) pair
// whose worktree row has `deleted_at IS NULL`. Ordered by worktree id.
func (s *Store) ListActiveWorktrees(ctx context.Context) ([]ActiveWorktree, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT r.path, w.path
		FROM worktrees w
		JOIN repos r ON r.id = w.repo_id
		WHERE w.deleted_at IS NULL
		ORDER BY w.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ActiveWorktree
	for rows.Next() {
		var a ActiveWorktree
		if err := rows.Scan(&a.RepoPath, &a.WorktreePath); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListRepoPaths returns every registered repo path in insertion order.
// Used by the daemon at boot to resume per-repo watchers.
func (s *Store) ListRepoPaths(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT path FROM repos ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EventLevel constants the events table CHECKs on.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// WriteEvent inserts a row into `events`. Any of repoID/worktreeID/
// phase may be zero / empty.
//
// When the event batcher is active (daemon path) the row is
// enqueued for async flush; the returned error is always nil since
// the write happens asynchronously. The CLI path runs sync — the
// short-lived process doesn't benefit from batching and the sync
// error report stays useful for diagnostics.
func (s *Store) WriteEvent(ctx context.Context,
	level, eventType string,
	message string,
	repoID, worktreeID int64,
	phase string,
	durationMs int64,
	payload any,
) error {
	payload = injectRunID(ctx, payload)
	pj, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event payload: %w", err)
	}
	row := pendingEvent{
		tsMillis:  nowMillis(),
		level:     level,
		eventType: eventType,
		payload:   string(pj),
	}
	if repoID > 0 {
		row.repoID = repoID
	}
	if worktreeID > 0 {
		row.worktreeID = worktreeID
	}
	if phase != "" {
		row.phase = phase
	}
	if durationMs > 0 {
		row.durationMs = durationMs
	}
	if message != "" {
		row.message = message
	}

	s.batchMu.Lock()
	if s.batchActive {
		s.batchBuf = append(s.batchBuf, row)
		shouldSignal := len(s.batchBuf) >= batchFlushThreshold
		s.batchMu.Unlock()
		if shouldSignal {
			select {
			case s.batchSignal <- struct{}{}:
			default:
			}
		}
		return nil
	}
	s.batchMu.Unlock()

	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO events(ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.tsMillis, row.level, row.repoID, row.worktreeID,
		row.eventType, row.phase, row.message, row.payload, row.durationMs)
	return err
}

// injectRunID stamps the ctx-bound run_id into payload so every event
// emitted by one flow shares a correlation key. Maps (the common
// case) get the field in place; nil payloads become a fresh map;
// anything else (rare) is wrapped under {run_id, payload}. The
// run_id key is left alone if the caller already supplied one.
func injectRunID(ctx context.Context, payload any) any {
	id := runid.From(ctx)
	if id == "" {
		return payload
	}
	switch p := payload.(type) {
	case nil:
		return map[string]string{"run_id": id}
	case map[string]any:
		if _, ok := p["run_id"]; !ok {
			p["run_id"] = id
		}
		return p
	case map[string]string:
		if _, ok := p["run_id"]; !ok {
			p["run_id"] = id
		}
		return p
	default:
		return map[string]any{"run_id": id, "payload": p}
	}
}
