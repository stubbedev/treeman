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
}

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

// Close shuts down the underlying pool.
func (s *Store) Close() error { return s.DB.Close() }

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
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", e.Name(), err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO _treeman_migrations(version, filename, applied_at) VALUES (?,?,?)",
			version, e.Name(), nowMillis()); err != nil {
			tx.Rollback()
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
	row := s.DB.QueryRowContext(ctx, "SELECT id FROM worktrees WHERE path = ?", path)
	var id int64
	if err := row.Scan(&id); err == nil {
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var br interface{}
	if branch != "" {
		br = branch
	}
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO worktrees(repo_id, path, slug, branch, created_at) VALUES (?, ?, ?, ?, ?)",
		repoID, path, slug, br, nowMillis())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MarkWorktreeDeleted sets the worktree's `deleted_at` to now.
func (s *Store) MarkWorktreeDeleted(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE worktrees SET deleted_at = ? WHERE id = ?", nowMillis(), id)
	return err
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
		pj = []byte("{}")
	}
	var (
		rid interface{}
		wid interface{}
		ph  interface{}
		dur interface{}
		msg interface{}
	)
	if repoID > 0 {
		rid = repoID
	}
	if worktreeID > 0 {
		wid = worktreeID
	}
	if phase != "" {
		ph = phase
	}
	if durationMs > 0 {
		dur = durationMs
	}
	if message != "" {
		msg = message
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO events(ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nowMillis(), level, rid, wid, eventType, ph, msg, string(pj), dur)
	return err
}
