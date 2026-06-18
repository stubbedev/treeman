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

// ActiveBranchRow is one active-namespace marker: which branch's data
// currently occupies `DBKey` for a worktree, and the engine family.
type ActiveBranchRow struct {
	DBKey  string `json:"db_key"`
	Branch string `json:"branch"`
	Engine string `json:"engine"`
}

// ListActiveBranches returns every active-namespace marker for a
// worktree, ordered by db_key for stable output. Empty (not nil) when
// the worktree has no branch_scoped databases swapped in yet, so callers
// can range without a nil check.
func (s *Store) ListActiveBranches(ctx context.Context, worktreeID int64) ([]ActiveBranchRow, error) {
	out := []ActiveBranchRow{}
	if worktreeID <= 0 {
		return out, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT db_key, branch, engine FROM active_branch_db WHERE worktree_id = ? ORDER BY db_key`,
		worktreeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r ActiveBranchRow
		if err := rows.Scan(&r.DBKey, &r.Branch, &r.Engine); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

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
//
// It always resets clean=0 / watermark=” — a marker advance means the
// active slot is being (re)filled, so the safe default is "dirty, must
// capture". The end-of-prepare SetActiveBranchClean sets the real clean
// state once the fill+migrate outcome is known. A crash between the two
// therefore leaves clean=0, which forces the next swap to capture rather
// than risk skipping a capture against stale bookkeeping.
func (s *Store) SetActiveBranch(ctx context.Context, repoID, worktreeID int64, dbKey, branch, engine string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO active_branch_db(repo_id, worktree_id, db_key, branch, engine, updated_at, clean, watermark)
		VALUES (?, ?, ?, ?, ?, ?, 0, '')
		ON CONFLICT(worktree_id, db_key) DO UPDATE SET
			branch     = excluded.branch,
			engine     = excluded.engine,
			updated_at = excluded.updated_at,
			clean      = 0,
			watermark  = ''`,
		repoID, worktreeID, dbKey, branch, engine, time.Now().UnixMilli())
	return err
}

// SetActiveBranchClean records the lever-1 capture-skip bookkeeping for
// `dbKey`'s marker: whether the active namespace currently mirrors the
// durable copy of `branch` (clean), and the write watermark observed at
// that moment. Called at the end of a successful prepare, after the marker
// has already been advanced by SetActiveBranch. clean=false stores an
// empty watermark so a stale token can never be matched.
func (s *Store) SetActiveBranchClean(
	ctx context.Context,
	repoID, worktreeID int64,
	dbKey, branch, engine string,
	clean bool,
	watermark string,
) error {
	c := 0
	wm := ""
	if clean {
		c = 1
		wm = watermark
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO active_branch_db(repo_id, worktree_id, db_key, branch, engine, updated_at, clean, watermark)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(worktree_id, db_key) DO UPDATE SET
			branch     = excluded.branch,
			engine     = excluded.engine,
			updated_at = excluded.updated_at,
			clean      = excluded.clean,
			watermark  = excluded.watermark`,
		repoID, worktreeID, dbKey, branch, engine, time.Now().UnixMilli(), c, wm)
	return err
}

// GetActiveBranchClean returns the capture-skip bookkeeping for `dbKey`'s
// marker: whether the active namespace is known clean (mirrors its branch's
// durable copy) and the watermark captured at that point. ok=false when no
// marker exists. A clean=false row reports clean=false with an empty
// watermark.
func (s *Store) GetActiveBranchClean(ctx context.Context, worktreeID int64, dbKey string) (bool, string, bool, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT clean, watermark FROM active_branch_db WHERE worktree_id = ? AND db_key = ?`,
		worktreeID, dbKey)
	var clean int
	var wm string
	switch err := row.Scan(&clean, &wm); {
	case errors.Is(err, sql.ErrNoRows):
		return false, "", false, nil
	case err != nil:
		return false, "", false, err
	default:
		return clean == 1, wm, true, nil
	}
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
