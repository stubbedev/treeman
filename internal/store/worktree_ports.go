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
		// (worktree, name). The former is recoverable (caller picks
		// the next port); the latter is a programming bug.
		if isUniqueConflict(err) {
			if held, herr := s.LookupWorktreePort(ctx, worktreeID, name); herr == nil && held > 0 {
				return fmt.Errorf("allocate worktree port: worktree %d already holds slot %q (port=%d)", worktreeID, name, held)
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
	defer rows.Close()
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
	defer rows.Close()
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
