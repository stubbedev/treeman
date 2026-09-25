package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// template_db_built tracks the input fingerprint at which a worktree's
// template-path namespaces (source + test clones) were last populated.
// See the 0013_template_db_built migration for the rationale. The
// cache-hit path reads it to skip a redundant template restore when the
// fingerprint is unchanged.

// GetTemplateBuilt returns the fingerprint recorded for (worktree,
// dbKey, engine), ok=false when no row exists (which the caller treats
// as "must restore").
func (s *Store) GetTemplateBuilt(ctx context.Context, worktreeID int64, dbKey, engine string) (string, bool, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT fingerprint FROM template_db_built WHERE worktree_id = ? AND db_key = ? AND engine = ?`,
		worktreeID, dbKey, engine)
	var fp string
	switch err := row.Scan(&fp); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	default:
		return fp, true, nil
	}
}

// SetTemplateBuilt upserts the built-at fingerprint for (worktree,
// dbKey, engine). Called after a successful restore/fan-out or cold
// build.
func (s *Store) SetTemplateBuilt(ctx context.Context, worktreeID int64, dbKey, engine, fingerprint string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO template_db_built(worktree_id, db_key, engine, fingerprint, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(worktree_id, db_key, engine) DO UPDATE SET
			fingerprint = excluded.fingerprint,
			updated_at  = excluded.updated_at`,
		worktreeID, dbKey, engine, fingerprint, time.Now().UnixMilli())
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
