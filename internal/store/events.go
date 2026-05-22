package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Event is one row from the `events` table — already-decoded so the
// CLI can format / filter without re-touching SQLite.
type Event struct {
	ID          int64
	Ts          int64 // unix-ms
	Level       string
	RepoID      sql.NullInt64
	WorktreeID  sql.NullInt64
	EventType   string
	Phase       string
	Message     string
	PayloadJSON string
	DurationMs  sql.NullInt64

	// Hydrated by joined queries when worktree info is requested.
	WorktreeSlug   string
	WorktreeBranch string
	WorktreePath   string
}

// EventFilter is the union of every predicate the `logs` CLI exposes.
// Empty fields are no-ops, so callers can construct it incrementally.
type EventFilter struct {
	WorktreeID  int64    // join filter — 0 = any
	RepoID      int64    // join filter — 0 = any
	Levels      []string // empty = any
	EventTypes  []string // empty = any
	Phases      []string // empty = any
	MessageLike string   // SQL LIKE pattern (substring match)
	PayloadLike string   // SQL LIKE pattern against payload_json
	SinceMs     int64    // ts >= SinceMs when non-zero
	UntilMs     int64    // ts <= UntilMs when non-zero
	AfterID     int64    // id > AfterID — for incremental follow
	Limit       int      // 0 = no LIMIT clause
	OldestFirst bool     // ORDER BY ts ASC instead of DESC
	HydrateWT   bool     // LEFT JOIN worktrees to fill WorktreeSlug etc.
}

// QueryEvents returns events matching f. The default ordering is
// newest-first; pass OldestFirst=true to stream chronologically (used
// by --follow and oldest-first tail output).
func (s *Store) QueryEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	cols := `e.id, e.ts, e.level, e.repo_id, e.worktree_id, e.event_type,
		COALESCE(e.phase,''), COALESCE(e.message,''), e.payload_json, e.duration_ms`
	from := `FROM events e`
	if f.HydrateWT {
		cols += `, COALESCE(w.slug,''), COALESCE(w.branch,''), COALESCE(w.path,'')`
		from += ` LEFT JOIN worktrees w ON w.id = e.worktree_id`
	}
	q := "SELECT " + cols + " " + from
	where := []string{}
	args := []any{}
	if f.WorktreeID > 0 {
		where = append(where, "e.worktree_id = ?")
		args = append(args, f.WorktreeID)
	}
	if f.RepoID > 0 {
		where = append(where, "e.repo_id = ?")
		args = append(args, f.RepoID)
	}
	if len(f.Levels) > 0 {
		where = append(where, "e.level IN ("+placeholders(len(f.Levels))+")")
		for _, v := range f.Levels {
			args = append(args, v)
		}
	}
	if len(f.EventTypes) > 0 {
		where = append(where, "e.event_type IN ("+placeholders(len(f.EventTypes))+")")
		for _, v := range f.EventTypes {
			args = append(args, v)
		}
	}
	if len(f.Phases) > 0 {
		where = append(where, "e.phase IN ("+placeholders(len(f.Phases))+")")
		for _, v := range f.Phases {
			args = append(args, v)
		}
	}
	if f.MessageLike != "" {
		where = append(where, "e.message LIKE ?")
		args = append(args, "%"+f.MessageLike+"%")
	}
	if f.PayloadLike != "" {
		where = append(where, "e.payload_json LIKE ?")
		args = append(args, "%"+f.PayloadLike+"%")
	}
	if f.SinceMs > 0 {
		where = append(where, "e.ts >= ?")
		args = append(args, f.SinceMs)
	}
	if f.UntilMs > 0 {
		where = append(where, "e.ts <= ?")
		args = append(args, f.UntilMs)
	}
	if f.AfterID > 0 {
		where = append(where, "e.id > ?")
		args = append(args, f.AfterID)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.OldestFirst {
		q += " ORDER BY e.ts ASC, e.id ASC"
	} else {
		q += " ORDER BY e.ts DESC, e.id DESC"
	}
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		dest := []any{&e.ID, &e.Ts, &e.Level, &e.RepoID, &e.WorktreeID, &e.EventType, &e.Phase, &e.Message, &e.PayloadJSON, &e.DurationMs}
		if f.HydrateWT {
			dest = append(dest, &e.WorktreeSlug, &e.WorktreeBranch, &e.WorktreePath)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LookupWorktreeID returns the worktree id whose basename, slug, or
// branch matches `name` (or 0 if no match) for an optional repo
// scope. Used by `logs tail --worktree NAME` to translate a user-
// facing handle into a SQL filter.
func (s *Store) LookupWorktreeID(ctx context.Context, repoID int64, name string) (int64, error) {
	q := `SELECT id FROM worktrees WHERE deleted_at IS NULL AND
		(slug = ? OR branch = ? OR path LIKE ?)`
	args := []any{name, name, "%/" + name}
	if repoID > 0 {
		q += " AND repo_id = ?"
		args = append(args, repoID)
	}
	q += " ORDER BY id DESC LIMIT 1"
	var id int64
	row := s.DB.QueryRowContext(ctx, q, args...)
	if err := row.Scan(&id); err != nil {
		return 0, nil
	}
	return id, nil
}

// HookRun is one row from the `hook_runs` table.
type HookRun struct {
	ID         int64
	WorktreeID int64
	Phase      string
	GroupIdx   int64
	Command    string
	StartedAt  int64 // unix-ms
	FinishedAt sql.NullInt64
	ExitCode   sql.NullInt64
	StdoutTail string
	StderrTail string
}

// QueryHookRuns returns the most recent hook executions for a
// worktree, newest first. Pass limit=0 for "no LIMIT".
func (s *Store) QueryHookRuns(ctx context.Context, worktreeID int64, limit int) ([]HookRun, error) {
	q := `SELECT id, worktree_id, phase, group_idx, COALESCE(command,''),
		started_at, finished_at, exit_code,
		COALESCE(stdout_tail,''), COALESCE(stderr_tail,'')
		FROM hook_runs WHERE worktree_id = ? ORDER BY started_at DESC, id DESC`
	args := []any{worktreeID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query hook_runs: %w", err)
	}
	defer rows.Close()
	var out []HookRun
	for rows.Next() {
		var h HookRun
		if err := rows.Scan(&h.ID, &h.WorktreeID, &h.Phase, &h.GroupIdx, &h.Command, &h.StartedAt, &h.FinishedAt, &h.ExitCode, &h.StdoutTail, &h.StderrTail); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// WriteHookRun records one hook-group execution. exitCode<0 / finishedMs==0
// indicate the row is for a still-running async group; the caller can
// follow up with UpdateHookRun once the driver exits, but for the
// current synchronous code paths every call here passes a final state.
func (s *Store) WriteHookRun(ctx context.Context,
	worktreeID int64,
	phase string,
	groupIdx int,
	command string,
	startedMs, finishedMs int64,
	exitCode int,
	stdoutTail, stderrTail string,
) error {
	if worktreeID <= 0 {
		return nil
	}
	var fin sql.NullInt64
	if finishedMs > 0 {
		fin = sql.NullInt64{Int64: finishedMs, Valid: true}
	}
	var exit sql.NullInt64
	if finishedMs > 0 {
		exit = sql.NullInt64{Int64: int64(exitCode), Valid: true}
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO hook_runs(worktree_id, phase, group_idx, command,
			started_at, finished_at, exit_code, stdout_tail, stderr_tail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		worktreeID, phase, groupIdx, command, startedMs, fin, exit, stdoutTail, stderrTail)
	if err != nil {
		return fmt.Errorf("insert hook_run: %w", err)
	}
	return nil
}

// placeholders returns "?, ?, ?" repeated n times.
// PurgeEvents deletes events matching the filter and returns the row
// count removed. Only SinceMs / UntilMs / RepoID / WorktreeID /
// EventTypes / Levels are honored — every other field on the filter
// is ignored. SQLite's DELETE is sufficient for the table sizes we
// expect (events accumulate, but a single repo rarely tops 100k rows).
func (s *Store) PurgeEvents(ctx context.Context, f EventFilter) (int64, error) {
	q := "DELETE FROM events"
	where := []string{}
	args := []any{}
	if f.RepoID > 0 {
		where = append(where, "repo_id = ?")
		args = append(args, f.RepoID)
	}
	if f.WorktreeID > 0 {
		where = append(where, "worktree_id = ?")
		args = append(args, f.WorktreeID)
	}
	if f.UntilMs > 0 {
		where = append(where, "ts <= ?")
		args = append(args, f.UntilMs)
	}
	if f.SinceMs > 0 {
		where = append(where, "ts >= ?")
		args = append(args, f.SinceMs)
	}
	if len(f.Levels) > 0 {
		where = append(where, "level IN ("+placeholders(len(f.Levels))+")")
		for _, v := range f.Levels {
			args = append(args, v)
		}
	}
	if len(f.EventTypes) > 0 {
		where = append(where, "event_type IN ("+placeholders(len(f.EventTypes))+")")
		for _, v := range f.EventTypes {
			args = append(args, v)
		}
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
