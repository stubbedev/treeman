package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// WorktreePortRow is one (worktree, name, port) assignment.
type WorktreePortRow struct {
	WorktreeID  int64
	RepoID      int64
	Name        string
	Port        uint16
	AllocatedAt int64
}

// AllocateWorktreePort inserts a (repo_id, worktree_id, name, port)
// row. Returns ErrPortInUse when the port is already held by another
// live worktree for the same (repo, slot) — caller should pick the
// next candidate in the range and retry.
//
// `name` is the declarative slot name from the top-level `ports:`
// block (e.g. "octane"). One worktree may not allocate the same
// slot twice; a re-insert for the same (worktree_id, name) fails
// loud so a buggy double-allocation surfaces immediately rather
// than silently overwriting.
func (s *Store) AllocateWorktreePort(ctx context.Context, repoID, worktreeID int64, name string, port uint16) error {
	if repoID <= 0 || worktreeID <= 0 || name == "" || port == 0 {
		return fmt.Errorf("allocate worktree port: bad args (repo=%d wt=%d name=%q port=%d)", repoID, worktreeID, name, port)
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO worktree_ports(repo_id, worktree_id, name, port, allocated_at)
		VALUES (?, ?, ?, ?, ?)`,
		repoID, worktreeID, name, port, nowMillis())
	if err != nil {
		// SQLite reports both unique-index conflicts as the same
		// constraint kind, so we need to distinguish via a follow-up
		// query whether the conflict was on (repo, name, port) or
		// (worktree, name). Both are recoverable:
		//
		//   - (worktree, name): this worktree already holds the slot.
		//     Happens when two allocate passes for the same worktree
		//     race — the synchronous create handler and the detached
		//     FinalizeWorktree both call Allocate, and the loser's
		//     INSERT lands after the winner's row. Idempotent, not a
		//     bug: signal the caller to reuse the recorded port.
		//   - (repo, name, port): another live worktree holds this
		//     exact port. Caller picks the next candidate.
		if isUniqueConflict(err) {
			if held, herr := s.LookupWorktreePort(ctx, worktreeID, name); herr == nil && held > 0 {
				return ErrSlotHeld
			}
			return ErrPortInUse
		}
		return err
	}
	return nil
}

// ErrPortInUse signals that a port is already allocated to another
// worktree. The allocator catches this, picks the next port in the
// range, and retries.
var ErrPortInUse = errors.New("port already allocated")

// ErrSlotHeld signals that THIS worktree already holds the slot — a
// concurrent allocate pass for the same worktree won the insert race.
// The allocator catches this and reuses the recorded port instead of
// failing the create.
var ErrSlotHeld = errors.New("worktree already holds slot")

// LookupWorktreePort returns the port assigned to (worktreeID, name)
// or 0 if no row matches.
func (s *Store) LookupWorktreePort(ctx context.Context, worktreeID int64, name string) (uint16, error) {
	if worktreeID <= 0 || name == "" {
		return 0, nil
	}
	row := s.DB.QueryRowContext(ctx,
		`SELECT port FROM worktree_ports WHERE worktree_id = ? AND name = ?`,
		worktreeID, name)
	var p uint16
	if err := row.Scan(&p); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return p, nil
}

// LoadWorktreePorts returns the full slot→port map for a worktree.
// Empty map (not nil) when no rows exist so callers can range
// without nil-checking.
func (s *Store) LoadWorktreePorts(ctx context.Context, worktreeID int64) (map[string]uint16, error) {
	out := map[string]uint16{}
	if worktreeID <= 0 {
		return out, nil
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name, port FROM worktree_ports WHERE worktree_id = ? ORDER BY name`,
		worktreeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var port uint16
		if err := rows.Scan(&name, &port); err != nil {
			return nil, err
		}
		out[name] = port
	}
	return out, rows.Err()
}

// ListUsedPorts returns the set of ports currently allocated to any
// live worktree under (repoID, name). Used by the allocator to skip
// over already-claimed ports when scanning the configured range.
//
// Soft-deleted worktrees are excluded: if a worktree was removed
// without `wt delete` cleaning up its rows (e.g. the daemon was
// killed), the join filter keeps those rows from blocking new
// allocations forever.
func (s *Store) ListUsedPorts(ctx context.Context, repoID int64, name string) (map[uint16]struct{}, error) {
	out := map[uint16]struct{}{}
	if repoID <= 0 || name == "" {
		return out, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT wp.port
		FROM worktree_ports wp
		JOIN worktrees w ON w.id = wp.worktree_id
		WHERE wp.repo_id = ? AND wp.name = ? AND w.deleted_at IS NULL`,
		repoID, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p uint16
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = struct{}{}
	}
	return out, rows.Err()
}

// ReleaseWorktreePorts drops every port row for the given worktree.
// Called as part of `wt delete` so the freed ports can be re-used by
// the next allocation.
func (s *Store) ReleaseWorktreePorts(ctx context.Context, worktreeID int64) error {
	if worktreeID <= 0 {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM worktree_ports WHERE worktree_id = ?`, worktreeID)
	return err
}

// ReleaseWorktreePort drops a single (worktree, slot) row. Used to roll
// back only the slots allocated in the current pass without disturbing
// ports the worktree already held — the all-slot ReleaseWorktreePorts
// would clobber pre-existing assignments on a partial-allocation retry.
func (s *Store) ReleaseWorktreePort(ctx context.Context, worktreeID int64, name string) error {
	if worktreeID <= 0 || name == "" {
		return nil
	}
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM worktree_ports WHERE worktree_id = ? AND name = ?`, worktreeID, name)
	return err
}

// PurgeDeletedWorktreePorts physically drops every port row whose
// worktree has been soft-deleted (deleted_at set) or whose worktree
// row is gone entirely. Returns the number of rows reaped.
//
// Per-delete teardown already calls ReleaseWorktreePorts, but rows can
// still leak when teardown is interrupted (daemon killed mid-teardown)
// or when a worktree was deleted by an older binary that predates that
// release. Leaked rows are invisible to ListUsedPorts (it filters on
// deleted_at IS NULL) yet the unique index on (repo_id, name, port)
// still rejects the re-insert, so the allocator climbs past every
// leaked port instead of reusing it. Sweeping on daemon boot keeps the
// allocation range from drifting upward indefinitely.
func (s *Store) PurgeDeletedWorktreePorts(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		DELETE FROM worktree_ports
		WHERE worktree_id IN (
			SELECT wp.worktree_id
			FROM worktree_ports wp
			LEFT JOIN worktrees w ON w.id = wp.worktree_id
			WHERE w.id IS NULL OR w.deleted_at IS NOT NULL
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SortedSlotNames returns the slot names of a port map in stable
// (alphabetical) order. Used by display layers (`wt show`,
// `wt create` summary line) so output doesn't shuffle between runs.
func SortedSlotNames(ports map[string]uint16) []string {
	if len(ports) == 0 {
		return nil
	}
	names := make([]string, 0, len(ports))
	for n := range ports {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// isUniqueConflict reports whether err is a SQLite UNIQUE-constraint
// violation. The modernc.org/sqlite driver surfaces this as an error
// whose message contains `UNIQUE constraint failed`; we match on the
// substring rather than the error code so the helper stays driver-
// agnostic (a future swap to mattn/go-sqlite3 keeps working).
func isUniqueConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
