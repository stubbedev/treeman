package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// template_db_built tracks the input fingerprint at which a worktree's
// template-path namespaces (source + test clones) were last populated.
// See the 0013_template_db_built migration for the rationale. The
// cache-hit path reads it to skip a redundant template restore when the
// fingerprint is unchanged; the incremental path reads its watermarks
// to migrate an untouched target forward in place (0022).

// TemplateBuilt is what a worktree's template-path namespaces were last
// populated with: the input fingerprint and, per target namespace, the
// engine write watermark observed right after that build. A target
// whose watermark still reads the same provably holds the fingerprint's
// state. A target absent from Watermarks proves nothing.
type TemplateBuilt struct {
	Fingerprint string
	Watermarks  map[string]string
}

// GetTemplateBuilt returns what was recorded for (worktree, dbKey,
// engine, connection), ok=false when no row exists (which the caller
// treats as "must restore").
func (s *Store) GetTemplateBuilt(ctx context.Context, worktreeID int64, dbKey, engine, connection string) (TemplateBuilt, bool, error) {
	return readTemplateBuilt(ctx, s.DB, worktreeID, dbKey, engine, connection)
}

// TakeTemplateBuilt returns what was recorded for (worktree, dbKey,
// engine, connection) and deletes it in the same transaction. A build
// takes the record before it writes to any target, so a build that
// fails part way leaves no record vouching for targets it half-wrote.
func (s *Store) TakeTemplateBuilt(ctx context.Context, worktreeID int64, dbKey, engine, connection string) (TemplateBuilt, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return TemplateBuilt{}, false, fmt.Errorf("begin taking the built-at record of %s %s: %w", engine, dbKey, err)
	}
	defer func() { _ = tx.Rollback() }()
	built, ok, err := readTemplateBuilt(ctx, tx, worktreeID, dbKey, engine, connection)
	if err != nil || !ok {
		return built, ok, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM template_db_built WHERE worktree_id = ? AND db_key = ? AND engine = ? AND connection = ?`,
		worktreeID, dbKey, engine, connection); err != nil {
		return TemplateBuilt{}, false, fmt.Errorf("delete the built-at record of %s %s: %w", engine, dbKey, err)
	}
	if err := tx.Commit(); err != nil {
		return TemplateBuilt{}, false, fmt.Errorf("commit taking the built-at record of %s %s: %w", engine, dbKey, err)
	}
	return built, true, nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readTemplateBuilt(
	ctx context.Context,
	q rowQuerier,
	worktreeID int64,
	dbKey, engine, connection string,
) (TemplateBuilt, bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT fingerprint, watermarks_json FROM template_db_built WHERE worktree_id = ? AND db_key = ? AND engine = ? AND connection = ?`,
		worktreeID, dbKey, engine, connection)
	var (
		built TemplateBuilt
		wm    string
	)
	switch err := row.Scan(&built.Fingerprint, &wm); {
	case errors.Is(err, sql.ErrNoRows):
		return TemplateBuilt{}, false, nil
	case err != nil:
		return TemplateBuilt{}, false, fmt.Errorf("read the built-at record of %s %s: %w", engine, dbKey, err)
	}
	if err := json.Unmarshal([]byte(wm), &built.Watermarks); err != nil {
		return TemplateBuilt{}, false, fmt.Errorf("decode watermarks_json of %s %s: %w", engine, dbKey, err)
	}
	return built, true, nil
}

// SetTemplateBuilt upserts what (worktree, dbKey, engine, connection)
// was built with, replacing every recorded watermark. Called after a
// successful restore/fan-out or build.
func (s *Store) SetTemplateBuilt(ctx context.Context, worktreeID int64, dbKey, engine, connection string, built TemplateBuilt) error {
	wm := []byte("{}")
	if len(built.Watermarks) > 0 {
		var err error
		if wm, err = json.Marshal(built.Watermarks); err != nil {
			return fmt.Errorf("encode the watermarks of %s %s: %w", engine, dbKey, err)
		}
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO template_db_built(worktree_id, db_key, engine, connection, fingerprint, watermarks_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(worktree_id, db_key, engine, connection) DO UPDATE SET
			fingerprint     = excluded.fingerprint,
			watermarks_json = excluded.watermarks_json,
			updated_at      = excluded.updated_at`,
		worktreeID, dbKey, engine, connection, built.Fingerprint, string(wm), time.Now().UnixMilli())
	return err
}

// ClearTemplateBuiltForKey drops the built-at fingerprint for one
// (worktree, dbKey) across engines. Called by `treeman db reset` so the
// next prepare re-populates the reset namespaces.
func (s *Store) ClearTemplateBuiltForKey(ctx context.Context, worktreeID int64, dbKey string) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM template_db_built WHERE worktree_id = ? AND db_key = ?`, worktreeID, dbKey)
	return err
}

// ClearTemplateBuiltForWorktree drops every built-at fingerprint for a
// worktree. Called by stale-prepare recovery, which resets the engine
// namespaces out from under any recorded fingerprints.
func (s *Store) ClearTemplateBuiltForWorktree(ctx context.Context, worktreeID int64) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM template_db_built WHERE worktree_id = ?`, worktreeID)
	return err
}
