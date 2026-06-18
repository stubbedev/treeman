package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// branch_db_migrated tracks the input fingerprint at which each branch's
// durable copy of a branch-scoped database was last migrated. See the
// 0009_branch_db_migrated migration for the rationale. The swap lifecycle
// reads it to skip a redundant `migrate` when a resumed branch's migration
// inputs are unchanged.

// GetBranchMigrated returns the fingerprint recorded for (worktree, dbKey,
// branch), ok=false when no row exists (which the caller treats as
// "must migrate").
func (s *Store) GetBranchMigrated(ctx context.Context, worktreeID int64, dbKey, branch string) (string, bool, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT fingerprint FROM branch_db_migrated WHERE worktree_id = ? AND db_key = ? AND branch = ?`,
		worktreeID, dbKey, branch)
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

// SetBranchMigrated upserts the migrated-at fingerprint for (worktree,
// dbKey, branch). Called after a successful migrate.
func (s *Store) SetBranchMigrated(ctx context.Context, worktreeID int64, dbKey, branch, fingerprint string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO branch_db_migrated(worktree_id, db_key, branch, fingerprint, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(worktree_id, db_key, branch) DO UPDATE SET
			fingerprint = excluded.fingerprint,
			updated_at  = excluded.updated_at`,
		worktreeID, dbKey, branch, fingerprint, time.Now().UnixMilli())
	return err
}

// ClearBranchMigratedForKey drops every branch's migrated-fingerprint for
// one (worktree, dbKey). Called by `treeman db reset` so the next prepare
// re-migrates the re-seeded namespace.
func (s *Store) ClearBranchMigratedForKey(ctx context.Context, worktreeID int64, dbKey string) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM branch_db_migrated WHERE worktree_id = ? AND db_key = ?`,
		worktreeID, dbKey)
	return err
}

// ClearBranchMigratedForWorktree drops every migrated-fingerprint for a
// worktree. Called on `wt delete` teardown.
func (s *Store) ClearBranchMigratedForWorktree(ctx context.Context, worktreeID int64) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM branch_db_migrated WHERE worktree_id = ?`, worktreeID)
	return err
}
