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
	RunID       string   // exact match against the run_id field inside payload_json
	SinceMs     int64    // ts >= SinceMs when non-zero
	UntilMs     int64    // ts <= UntilMs when non-zero
	AfterID     int64    // id > AfterID — for incremental follow
	Limit       int      // 0 = no LIMIT clause
	OldestFirst bool     // ORDER BY ts ASC instead of DESC
	HydrateWT   bool     // LEFT JOIN worktrees to fill WorktreeSlug etc.
}

// queryEventsWhere builds the WHERE-clause fragments and their bound
// args for QueryEvents. Split out so QueryEvents stays under the
// cyclomatic-complexity gate; the predicate ordering is preserved.
func queryEventsWhere(f EventFilter) (where []string, args []any) {
	where = []string{}
	args = []any{}
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
	if f.RunID != "" {
		// run_id lives inside payload_json as `"run_id":"<value>"`.
		// Exact match on the JSON-quoted form sidesteps the cost of
		// json_extract() and works on every SQLite build (json1 is
		// usually but not always compiled in for embedded sqlite).
		where = append(where, "e.payload_json LIKE ?")
		args = append(args, `%"run_id":"`+f.RunID+`"%`)
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
	return where, args
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
	where, args := queryEventsWhere(f)
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // only placeholder fragments joined; values are parameterized
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
	defer func() { _ = rows.Close() }()
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

// WorktreeMatch is one candidate row returned by LookupWorktreeMatches.
// Kind records WHICH column produced the hit so callers can apply
// match-rank precedence (branch beats slug, slug beats path).
type WorktreeMatch struct {
	ID     int64
	Slug   string
	Branch string
	Path   string
	Kind   string // "slug" | "branch" | "basename"
}

// LookupWorktreeID returns the worktree id whose slug, branch, or
// basename matches `name` (or 0 if no match). With ambiguous slugs
// (two tickets, two worktrees, one slug) this surfaces a non-nil
// error AND a zero id so callers can render the candidate list
// instead of the historic misleading "no worktree matches".
//
// Match ranking: exact branch > exact slug > basename. Multiple
// rows tying at the same rank produce an ambiguous-match error;
// otherwise the top-ranked row wins regardless of how many rows
// match at lower ranks (a branch hit beats N slug hits).
func (s *Store) LookupWorktreeID(ctx context.Context, repoID int64, name string) (int64, error) {
	matches, err := s.LookupWorktreeMatches(ctx, repoID, name)
	if err != nil {
		return 0, err
	}
	if len(matches) == 0 {
		return 0, nil
	}
	winners := pickByRank(matches)
	if len(winners) == 1 {
		return winners[0].ID, nil
	}
	return 0, ambiguousMatchError(name, winners)
}

// LookupWorktreeMatches returns every active worktree row whose
// slug, branch, or basename matches `name`. Rows are returned newest-
// id first within each match kind; callers that want a single winner
// should run them through `pickByRank` (see LookupWorktreeID).
//
// Used by the lookup paths that need to surface candidates on
// ambiguity instead of silently picking one.
func (s *Store) LookupWorktreeMatches(ctx context.Context, repoID int64, name string) ([]WorktreeMatch, error) {
	q := `SELECT id, slug, COALESCE(branch,''), path,
		     CASE
		       WHEN branch = ?       THEN 'branch'
		       WHEN slug = ?         THEN 'slug'
		       ELSE                       'basename'
		     END AS kind
		FROM worktrees WHERE deleted_at IS NULL AND
		(slug = ? OR branch = ? OR path LIKE ? COLLATE NOCASE)`
	args := []any{name, name, name, name, "%/" + name}
	if repoID > 0 {
		q += " AND repo_id = ?"
		args = append(args, repoID)
	}
	q += " ORDER BY id DESC"
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WorktreeMatch
	for rows.Next() {
		var m WorktreeMatch
		if err := rows.Scan(&m.ID, &m.Slug, &m.Branch, &m.Path, &m.Kind); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// pickByRank returns the subset of matches at the highest-priority
// kind (branch > slug > basename). Ties at the top rank are returned
// verbatim so callers can detect ambiguity.
func pickByRank(matches []WorktreeMatch) []WorktreeMatch {
	for _, kind := range []string{"branch", "slug", "basename"} {
		var hit []WorktreeMatch
		for _, m := range matches {
			if m.Kind == kind {
				hit = append(hit, m)
			}
		}
		if len(hit) > 0 {
			return hit
		}
	}
	return nil
}

// ambiguousMatchError formats the "you asked for X, here are the
// candidates" message used when the lookup ranking left more than
// one row tied at the top. Worktree paths are included so the user
// can disambiguate by typing a unique branch or path component.
func ambiguousMatchError(name string, matches []WorktreeMatch) error {
	var b strings.Builder
	b.WriteString("ambiguous worktree ")
	fmt.Fprintf(&b, "%q", name)
	b.WriteString(" — ")
	fmt.Fprintf(&b, "%d candidates:", len(matches))
	for _, m := range matches {
		b.WriteString("\n  ")
		fmt.Fprintf(&b, "id=%d slug=%s branch=%s path=%s", m.ID, m.Slug, m.Branch, m.Path)
	}
	b.WriteString("\n(pass a more specific branch name or path to disambiguate)")
	return fmt.Errorf("%s", b.String())
}

// HookRun is one row from the `hook_runs` table.
type HookRun struct {
	ID           int64
	WorktreeID   int64
	WorktreeSlug string // hydrated by QueryHookRuns via LEFT JOIN
	Phase        string
	GroupIdx     int64
	Command      string
	StartedAt    int64 // unix-ms
	FinishedAt   sql.NullInt64
	ExitCode     sql.NullInt64
	StdoutTail   string
	StderrTail   string
}

// QueryHookRuns returns the most recent hook executions, newest
// first. Pass worktreeID<=0 to span every worktree; pass limit=0
// for "no LIMIT".
func (s *Store) QueryHookRuns(ctx context.Context, worktreeID int64, limit int) ([]HookRun, error) {
	q := `SELECT h.id, h.worktree_id, COALESCE(w.slug,''), h.phase, h.group_idx, COALESCE(h.command,''),
		h.started_at, h.finished_at, h.exit_code,
		COALESCE(h.stdout_tail,''), COALESCE(h.stderr_tail,'')
		FROM hook_runs h LEFT JOIN worktrees w ON w.id = h.worktree_id`
	args := []any{}
	if worktreeID > 0 {
		q += " WHERE h.worktree_id = ?"
		args = append(args, worktreeID)
	}
	q += " ORDER BY h.started_at DESC, h.id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query hook_runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []HookRun
	for rows.Next() {
		var h HookRun
		if err := rows.Scan(
			&h.ID,
			&h.WorktreeID,
			&h.WorktreeSlug,
			&h.Phase,
			&h.GroupIdx,
			&h.Command,
			&h.StartedAt,
			&h.FinishedAt,
			&h.ExitCode,
			&h.StdoutTail,
			&h.StderrTail,
		); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// WriteHookRun records one hook-group execution and returns the new
// row's id so callers can attach log chunks. exitCode<0 /
// finishedMs==0 indicate the row is for a still-running async group;
// for the current synchronous code paths every call here passes a
// final state.
//
// Returns (0, nil) when worktreeID <= 0 so non-daemon callers can
// opt in to persistence gradually without checking themselves.
func (s *Store) WriteHookRun(ctx context.Context,
	worktreeID int64,
	phase string,
	groupIdx int,
	command string,
	startedMs, finishedMs int64,
	exitCode int,
	stdoutTail, stderrTail string,
) (int64, error) {
	if worktreeID <= 0 {
		return 0, nil
	}
	var fin sql.NullInt64
	if finishedMs > 0 {
		fin = sql.NullInt64{Int64: finishedMs, Valid: true}
	}
	var exit sql.NullInt64
	if finishedMs > 0 {
		exit = sql.NullInt64{Int64: int64(exitCode), Valid: true}
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO hook_runs(worktree_id, phase, group_idx, command,
			started_at, finished_at, exit_code, stdout_tail, stderr_tail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		worktreeID, phase, groupIdx, command, startedMs, fin, exit, stdoutTail, stderrTail)
	if err != nil {
		return 0, fmt.Errorf("insert hook_run: %w", err)
	}
	return res.LastInsertId()
}

// HookLogChunk is one row from `hook_log_chunks` — a buffered slice of
// stdout / stderr / merged output captured during the hook run.
type HookLogChunk struct {
	ID        int64
	HookRunID int64
	Ts        int64
	Stream    string // 'stdout' | 'stderr' | 'merged'
	Body      []byte
}

// AppendHookLogChunk persists one chunk of captured output for a hook
// run. The body is stored verbatim — ANSI escapes are preserved so a
// later `treeman logs hooks show <id>` round-trips terminal colors.
func (s *Store) AppendHookLogChunk(ctx context.Context, hookRunID int64, stream string, body []byte) error {
	if hookRunID <= 0 || len(body) == 0 {
		return nil
	}
	if stream == "" {
		stream = "merged"
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO hook_log_chunks(hook_run_id, ts, stream, body)
		 VALUES (?, ?, ?, ?)`,
		hookRunID, nowMillis(), stream, body)
	if err != nil {
		return fmt.Errorf("insert hook_log_chunk: %w", err)
	}
	return nil
}

// QueryHookLog returns every chunk for hookRunID in insertion order.
// Callers stream the bodies to stdout to render the captured output
// (with ANSI escapes intact).
func (s *Store) QueryHookLog(ctx context.Context, hookRunID int64) ([]HookLogChunk, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, hook_run_id, ts, stream, body
		   FROM hook_log_chunks WHERE hook_run_id = ? ORDER BY id ASC`,
		hookRunID)
	if err != nil {
		return nil, fmt.Errorf("query hook_log_chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []HookLogChunk
	for rows.Next() {
		var c HookLogChunk
		if err := rows.Scan(&c.ID, &c.HookRunID, &c.Ts, &c.Stream, &c.Body); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneOldLogs removes rows older than cutoffMs from events,
// hook_runs (and via FK cascade hook_log_chunks). Returns total rows
// removed across all three tables. A cutoff <= 0 is a no-op so
// callers can guard "retention disabled" without branching.
func (s *Store) PruneOldLogs(ctx context.Context, cutoffMs int64) (int64, error) {
	if cutoffMs <= 0 {
		return 0, nil
	}
	var total int64
	for _, q := range []struct {
		stmt string
		col  string
	}{
		{"DELETE FROM events WHERE ts < ?", "events.ts"},
		{"DELETE FROM hook_runs WHERE started_at < ?", "hook_runs.started_at"},
	} {
		res, err := s.DB.ExecContext(ctx, q.stmt, cutoffMs)
		if err != nil {
			return total, fmt.Errorf("prune %s: %w", q.col, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	return total, nil
}

// placeholders returns "?, ?, ?" repeated n times.
// PurgeEvents deletes events matching the filter and returns the row
// count removed. Only SinceMs / UntilMs / RepoID / WorktreeID /
// EventTypes / Levels are honored — every other field on the filter
// is ignored. SQLite's DELETE is sufficient for the table sizes we
// expect (events accumulate, but a single repo rarely tops 100k rows).
func (s *Store) PurgeEvents(ctx context.Context, f EventFilter) (int64, error) {
	q := "DELETE FROM events"
	where, args := purgeEventsWhere(f)
	q = appendWhere(q, where)
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountEvents reports how many event rows currently match f. Shares
// the predicate set with PurgeEvents so a dry-run preview ("how many
// rows would the same filter purge?") matches the destructive call
// exactly. Returns 0 + nil on no-rows; the caller distinguishes via
// the err return.
func (s *Store) CountEvents(ctx context.Context, f EventFilter) (int64, error) {
	q := "SELECT COUNT(*) FROM events"
	where, args := purgeEventsWhere(f)
	q = appendWhere(q, where)
	var n int64
	if err := s.DB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// purgeEventsWhere is the shared WHERE-clause builder for PurgeEvents
// + CountEvents. Mirrors the predicate set in queryEventsWhere but
// without the joined-worktree filters that PurgeEvents doesn't apply.
func purgeEventsWhere(f EventFilter) (where []string, args []any) {
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
	return where, args
}

// appendWhere appends a WHERE clause to q when fragments is non-empty.
// Centralised so PurgeEvents + CountEvents share the same concatenation
// site (and the same single gosec exemption — every fragment is a
// hard-coded predicate template; user input only flows through the
// args slice).
func appendWhere(q string, fragments []string) string {
	if len(fragments) == 0 {
		return q
	}
	return q + " WHERE " + strings.Join(fragments, " AND ")
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
