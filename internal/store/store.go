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
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
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
	batchMu        sync.Mutex
	batchBuf       []pendingEvent
	batchSignal    chan struct{}
	batchCtx       context.Context
	batchCancel    context.CancelFunc
	batchDone      chan struct{}
	batchActive    bool
}

// pendingEvent is one buffered events-table row awaiting flush.
type pendingEvent struct {
	tsMillis   int64
	level      string
	repoID     interface{}
	worktreeID interface{}
	eventType  string
	phase      interface{}
	message    interface{}
	payload    string
	durationMs interface{}
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
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}
	db.SetMaxOpenConns(8)
	if err := migrate(ctx, db); err != nil {
		db.Close()
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
	go s.eventBatchLoop()
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
	// the goroutine observed it.
	s.flushEvents(context.Background())
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
			s.flushEvents(context.Background())
			return
		case <-ticker.C:
			s.flushEvents(s.batchCtx)
		case <-s.batchSignal:
			s.flushEvents(s.batchCtx)
		}
	}
}

// flushEvents commits every buffered row in one transaction. Errors
// are logged via the database driver path but never returned — the
// batch path is fire-and-forget by design (callers used `_ =
// WriteEvent(...)` everywhere). One bad row poisoning a whole batch
// would lose unrelated events, so we fall back to a per-row insert
// when the transaction fails.
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
			s.flushEventsFallback(ctx, batch)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.flushEventsFallback(ctx, batch)
	}
}

// flushEventsFallback re-inserts a batch one row at a time when the
// batched transaction fails. Slower but resilient: a single
// constraint violation can't lose the rest of the buffer.
func (s *Store) flushEventsFallback(ctx context.Context, batch []pendingEvent) {
	const stmt = `INSERT INTO events(ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, e := range batch {
		_, _ = s.DB.ExecContext(ctx, stmt,
			e.tsMillis, e.level, e.repoID, e.worktreeID, e.eventType,
			e.phase, e.message, e.payload, e.durationMs)
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
				return fmt.Errorf("apply migration %s: %w (rollback: %v)", e.Name(), err, rbErr)
			}
			return fmt.Errorf("apply migration %s: %w", e.Name(), err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO _treeman_migrations(version, filename, applied_at) VALUES (?,?,?)",
			version, e.Name(), nowMillis()); err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				return fmt.Errorf("%w (rollback: %v)", err, rbErr)
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
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM repos WHERE path = ?", path)
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
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM worktrees WHERE path = ?", path)
	var id int64
	if err := row.Scan(&id); err == nil {
		if adminDir != "" {
			_, _ = s.DB.ExecContext(ctx,
				"UPDATE worktrees SET admin_dir = ? WHERE id = ? AND (admin_dir IS NULL OR admin_dir != ?)",
				adminDir, id, adminDir)
		}
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var br interface{}
	if branch != "" {
		br = branch
	}
	var ad interface{}
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

// LookupWorktreeByAdminDir resolves an admin_dir back to the worktree
// row. Returns 0 + nil when no row matches.
func (s *Store) LookupWorktreeByAdminDir(ctx context.Context, adminDir string) (WorktreeRow, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, repo_id, path, slug, COALESCE(branch, ''), COALESCE(admin_dir, ''), deleted_at IS NOT NULL
		FROM worktrees WHERE admin_dir = ? ORDER BY id DESC LIMIT 1`, adminDir)
	var w WorktreeRow
	var deleted bool
	if err := row.Scan(&w.ID, &w.RepoID, &w.Path, &w.Slug, &w.Branch, &w.AdminDir, &deleted); err != nil {
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
}

// ListWorktreesForRepo returns every worktree row attached to repoID,
// including deleted ones. Used by the lifecycle reconcile pass to
// diff the DB against the filesystem.
func (s *Store) ListWorktreesForRepo(ctx context.Context, repoID int64) ([]WorktreeRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, repo_id, path, slug, COALESCE(branch, ''), COALESCE(admin_dir, ''), deleted_at IS NOT NULL
		FROM worktrees WHERE repo_id = ? ORDER BY id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorktreeRow
	for rows.Next() {
		var w WorktreeRow
		if err := rows.Scan(&w.ID, &w.RepoID, &w.Path, &w.Slug, &w.Branch, &w.AdminDir, &w.Deleted); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetRepoWatchLifecycle toggles the per-repo lifecycle-watcher opt-in
// flag. The watcher only acts on repos where this is 1 AND the global
// `worktrees.hook_lifecycle` config bool is true.
func (s *Store) SetRepoWatchLifecycle(ctx context.Context, repoID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := s.DB.ExecContext(ctx,
		"UPDATE repos SET watch_lifecycle = ? WHERE id = ?", v, repoID)
	return err
}

// GetRepoWatchLifecycle returns the current opt-in flag for repoID.
func (s *Store) GetRepoWatchLifecycle(ctx context.Context, repoID int64) (bool, error) {
	row := s.DB.QueryRowContext(ctx, "SELECT watch_lifecycle FROM repos WHERE id = ?", repoID)
	var v int
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return v != 0, nil
}

// ListLifecycleWatchedRepos returns every repo (path + id) where
// watch_lifecycle = 1. Used by the daemon at boot to subscribe.
func (s *Store) ListLifecycleWatchedRepos(ctx context.Context) ([]RepoRef, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT id, path FROM repos WHERE watch_lifecycle = 1 ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM worktrees WHERE path = ? AND deleted_at IS NULL", path)
	var id int64
	if err := row.Scan(&id); err != nil {
		return nil
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

// LookupRepoID resolves a repo path to its row id. Returns 0 + nil
// when no row matches (caller treats this as "unknown").
func (s *Store) LookupRepoID(ctx context.Context, path string) (int64, error) {
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM repos WHERE path = ?", path)
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
	defer rows.Close()
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
	defer rows.Close()
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
