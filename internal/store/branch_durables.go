package store

import (
	"context"

	"github.com/stubbedev/treeman/internal/engine"
)

// The branch_durables table records every branch_scoped durable copy (the
// per-branch preserved namespace a swap captures). See the
// 0012_branch_durables migration for the full rationale — in short, it makes
// the durable pool enumerable so an orphan sweep can drop durables whose
// branch is gone, which the one-way-hash forward reaper structurally cannot.

// BranchDurableRow is one tracked branch_scoped durable copy.
type BranchDurableRow struct {
	RepoID      int64
	WorktreeID  int64 // 0 when the owning worktree row was deleted (FK SET NULL)
	Engine      string
	DBKey       string // the active namespace this durable was captured from
	Branch      string
	DurableName string
	CreatedAt   int64
	LastUsedAt  int64
}

// RecordBranchDurable upserts the tracking row for a durable copy captured at
// `DurableName`. Engine is canonicalised on write to match the snapshots-table
// convention so downstream readers see the canonical Family label regardless
// of which alias the config used. Called after every successful Capture.
func (s *Store) RecordBranchDurable(ctx context.Context, r BranchDurableRow) error {
	if fam, ok := engine.Canonical(r.Engine); ok {
		r.Engine = string(fam)
	}
	now := nowMillis()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	r.LastUsedAt = now
	var wtID any
	if r.WorktreeID > 0 {
		wtID = r.WorktreeID
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO branch_durables(repo_id, worktree_id, engine, db_key, branch,
		                            durable_name, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, durable_name) DO UPDATE SET
			worktree_id  = excluded.worktree_id,
			engine       = excluded.engine,
			db_key       = excluded.db_key,
			branch       = excluded.branch,
			last_used_at = excluded.last_used_at`,
		r.RepoID, wtID, r.Engine, r.DBKey, r.Branch,
		r.DurableName, r.CreatedAt, r.LastUsedAt)
	return err
}

// DeleteBranchDurable removes the tracking row for one durable, by name.
// Called after the engine-side durable is dropped (reset, reap). Idempotent.
func (s *Store) DeleteBranchDurable(ctx context.Context, repoID int64, durableName string) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM branch_durables WHERE repo_id = ? AND durable_name = ?`,
		repoID, durableName)
	return err
}

// ListAllDurableNamesByEngine returns the set of durable_name values across
// EVERY repo for the given engine (canonicalised to match the write-side
// convention). The ES orphan reconcile uses it as its keep-set without per-repo
// scoping: a treeman ES cluster can be shared by several repos, and an index
// family is an orphan only if NO repo tracks it — so building the keep-set
// repo-wide is what stops one repo's sweep from dropping another repo's durable.
func (s *Store) ListAllDurableNamesByEngine(ctx context.Context, eng string) (map[string]struct{}, error) {
	if fam, ok := engine.Canonical(eng); ok {
		eng = string(fam)
	}
	out := map[string]struct{}{}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT durable_name FROM branch_durables WHERE engine = ?`, eng)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = struct{}{}
	}
	return out, rows.Err()
}

// ListBranchDurables returns every tracked durable for a repo, ordered by
// branch then durable_name for stable output. Empty (not nil) when none, so
// callers can range without a nil check. repoID 0 returns empty (guards
// against dropping rows that predate repo_id being populated).
func (s *Store) ListBranchDurables(ctx context.Context, repoID int64) ([]BranchDurableRow, error) {
	out := []BranchDurableRow{}
	if repoID == 0 {
		return out, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT repo_id, COALESCE(worktree_id, 0), engine, db_key, branch,
		       durable_name, created_at, last_used_at
		FROM branch_durables WHERE repo_id = ? ORDER BY branch, durable_name`, repoID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r BranchDurableRow
		if err := rows.Scan(&r.RepoID, &r.WorktreeID, &r.Engine, &r.DBKey,
			&r.Branch, &r.DurableName, &r.CreatedAt, &r.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
