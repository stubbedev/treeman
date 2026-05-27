package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The active_branch_db table records which branch's data currently
// occupies a branch-scoped database's active namespace (`db_key`).
// See the 0006_active_branch_db migration for the full rationale.

// GetActiveBranch returns the branch currently occupying `dbKey`'s
// active namespace for `worktreeID`, and ok=false when no marker exists
// (the active slot has never been swapped into for this worktree).
func (s *Store) GetActiveBranch(ctx context.Context, worktreeID int64, dbKey string) (string, bool, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT branch FROM active_branch_db WHERE worktree_id = ? AND db_key = ?`,
		worktreeID, dbKey)
	var branch string
	switch err := row.Scan(&branch); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	default:
		return branch, true, nil
	}
}

// SetActiveBranch upserts the marker so `dbKey`'s active namespace is
// recorded as holding `branch`'s data. Called after every successful
// swap-in (and on first-enable adoption).
func (s *Store) SetActiveBranch(ctx context.Context, repoID, worktreeID int64, dbKey, branch, engine string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO active_branch_db(repo_id, worktree_id, db_key, branch, engine, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(worktree_id, db_key) DO UPDATE SET
			branch     = excluded.branch,
			engine     = excluded.engine,
			updated_at = excluded.updated_at`,
		repoID, worktreeID, dbKey, branch, engine, time.Now().UnixMilli())
	return err
}

// ClearActiveBranch drops the marker for one (worktree, dbKey). Used by
// `treeman db reset` so the next prepare re-seeds from the parent
// branch rather than treating the active slot as already-occupied.
func (s *Store) ClearActiveBranch(ctx context.Context, worktreeID int64, dbKey string) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM active_branch_db WHERE worktree_id = ? AND db_key = ?`,
		worktreeID, dbKey)
	return err
}

// ClearActiveBranchesForWorktree drops every marker for a worktree.
// Called on `wt delete` teardown so a re-created worktree at the same
// path doesn't inherit a stale active-branch pointer.
func (s *Store) ClearActiveBranchesForWorktree(ctx context.Context, worktreeID int64) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM active_branch_db WHERE worktree_id = ?`, worktreeID)
	return err
}
