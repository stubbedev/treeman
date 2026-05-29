package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/stubbedev/treeman/internal/engine"
)

// FileHash is one entry in an InputVector: the relpath of a file under
// an input glob plus its content hash. Ordered by Path within a vector
// so prefix comparisons are well-defined.
type FileHash struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// InputVector is the ordered per-input file list (path + content hash)
// captured at snapshot time. Used by FindAncestorSnapshot to detect a
// cached template whose vectors are prefixes of the current run's —
// the building block for incremental cold builds.
type InputVector []FileHash

// SnapshotRecord mirrors a row in the `snapshots` table.
type SnapshotRecord struct {
	Fingerprint    string
	Engine         string
	EngineVersion  string
	SourceDB       string
	TemplateName   string
	MigrationsHash string
	DumpHash       string
	LockfileHashes map[string]string
	// Inputs is the per-input ordered file vector captured at build
	// time. Keys are input globs (matching the keys used in
	// LockfileHashes). Empty for snapshots predating the inputs_json
	// column (those can't participate as ancestors).
	Inputs     map[string]InputVector
	SizeBytes  int64
	CreatedAt  int64
	LastUsedAt int64
	UseCount   int64
	RepoID     int64
}

// LookupSnapshot returns the snapshot row for `fingerprint`, or
// (nil, nil) if no row exists. Errors only on DB faults.
func (s *Store) LookupSnapshot(ctx context.Context, fingerprint string) (*SnapshotRecord, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT fingerprint, engine, engine_version, source_db, template_name,
		       migrations_hash, COALESCE(dump_hash,''), COALESCE(lockfile_hashes_json,'{}'),
		       COALESCE(inputs_json,'{}'),
		       COALESCE(size_bytes,0), created_at, last_used_at, use_count
		FROM snapshots WHERE fingerprint = ?`, fingerprint)
	var r SnapshotRecord
	var lockJSON, inputsJSON string
	err := row.Scan(&r.Fingerprint, &r.Engine, &r.EngineVersion, &r.SourceDB,
		&r.TemplateName, &r.MigrationsHash, &r.DumpHash, &lockJSON, &inputsJSON,
		&r.SizeBytes, &r.CreatedAt, &r.LastUsedAt, &r.UseCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lockJSON != "" {
		if err := json.Unmarshal([]byte(lockJSON), &r.LockfileHashes); err != nil {
			return nil, fmt.Errorf("decode lockfile_hashes_json for %s: %w", fingerprint, err)
		}
	}
	if inputsJSON != "" && inputsJSON != "{}" {
		if err := json.Unmarshal([]byte(inputsJSON), &r.Inputs); err != nil {
			return nil, fmt.Errorf("decode inputs_json for %s: %w", fingerprint, err)
		}
	}
	return &r, nil
}

// RecordSnapshot inserts (or updates on conflict) the snapshots row
// for a freshly built template. `sizeBytes` may be 0 if unknown.
//
// Engine is canonicalised on write so downstream switches (gc.go,
// branchstate.go, MCP tools) read the canonical Family label
// regardless of which alias the user wrote in `databases[].engine`.
// Pre-canonical rows still work because every reader runs
// engine.Canonical, but new writes converge on one form.
func (s *Store) RecordSnapshot(ctx context.Context, r SnapshotRecord) error {
	if fam, ok := engine.Canonical(r.Engine); ok {
		r.Engine = string(fam)
	}
	if r.LockfileHashes == nil {
		r.LockfileHashes = map[string]string{}
	}
	lockJSON, err := json.Marshal(r.LockfileHashes)
	if err != nil {
		return fmt.Errorf("encode lockfile_hashes: %w", err)
	}
	if r.Inputs == nil {
		r.Inputs = map[string]InputVector{}
	}
	inputsJSON, err := json.Marshal(r.Inputs)
	if err != nil {
		return fmt.Errorf("encode inputs_json: %w", err)
	}
	now := nowMillis()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.LastUsedAt == 0 {
		r.LastUsedAt = now
	}
	var repoID any
	if r.RepoID > 0 {
		repoID = r.RepoID
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO snapshots(fingerprint, engine, engine_version, source_db,
		                      template_name, migrations_hash, dump_hash,
		                      lockfile_hashes_json, inputs_json,
		                      size_bytes, created_at,
		                      last_used_at, use_count, repo_id)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?,''), ?, ?, NULLIF(?,0), ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
		    template_name        = excluded.template_name,
		    engine_version       = excluded.engine_version,
		    source_db            = excluded.source_db,
		    migrations_hash      = excluded.migrations_hash,
		    dump_hash             = excluded.dump_hash,
		    lockfile_hashes_json = excluded.lockfile_hashes_json,
		    inputs_json          = excluded.inputs_json,
		    size_bytes           = excluded.size_bytes,
		    last_used_at         = excluded.last_used_at,
		    repo_id              = excluded.repo_id`,
		r.Fingerprint, r.Engine, r.EngineVersion, r.SourceDB,
		r.TemplateName, r.MigrationsHash, r.DumpHash, string(lockJSON), string(inputsJSON),
		r.SizeBytes, r.CreatedAt, r.LastUsedAt, r.UseCount, repoID)
	return err
}

// CommandsHashKey is the well-known LockfileHashes map key that
// computeSnapshotKey stuffs the migrate+seed run/env hash under. It's
// the only non-input fold-in today; pulling it out as a constant lets
// FindAncestorSnapshot compare commands explicitly without having to
// distinguish "real input glob" entries from this synthetic one.
const CommandsHashKey = "__commands__"

// FindAncestorSnapshot returns the cached snapshot that is the LONGEST
// content-prefix ancestor of the current run, or (nil, nil) when none
// qualifies. An ancestor is a row in the same repo with identical
// (engine, engine_version, dump_hash, commands_hash) where every input
// glob present in the candidate also appears in the current run AND
// the candidate's vector is a PREFIX of the current run's vector for
// that glob. The current run may carry additional entries at the end
// of any vector or additional input globs entirely — those represent
// the new files an incremental migrate will apply on top of the
// ancestor template.
//
// "Best" = the candidate whose Inputs collectively cover the most
// files (longest total vector). Returns (nil, nil) when no candidate
// is a STRICT ancestor — exact-equality matches are the
// LookupSnapshot cache-hit path, not an incremental case.
//
// Pure read: callers decide whether to act on the result.
func (s *Store) FindAncestorSnapshot(
	ctx context.Context,
	repoID int64,
	eng, engineVersion, dumpHash, currentCommandsHash string,
	currentInputs map[string]InputVector,
) (*SnapshotRecord, error) {
	if fam, ok := engine.Canonical(eng); ok {
		eng = string(fam)
	}
	dumpArg := sql.NullString{String: dumpHash, Valid: dumpHash != ""}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT fingerprint, engine, engine_version, source_db, template_name,
		       migrations_hash, COALESCE(dump_hash,''), COALESCE(lockfile_hashes_json,'{}'),
		       COALESCE(inputs_json,'{}'),
		       COALESCE(size_bytes,0), created_at, last_used_at, use_count
		FROM snapshots
		WHERE repo_id = ?
		  AND engine = ?
		  AND engine_version = ?
		  AND COALESCE(dump_hash,'') = COALESCE(?,'')`, repoID, eng, engineVersion, dumpArg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	currentTotal := vectorTotalLen(currentInputs)
	var best *SnapshotRecord
	bestLen := -1
	for rows.Next() {
		var r SnapshotRecord
		var lockJSON, inputsJSON string
		if err := rows.Scan(&r.Fingerprint, &r.Engine, &r.EngineVersion, &r.SourceDB,
			&r.TemplateName, &r.MigrationsHash, &r.DumpHash, &lockJSON, &inputsJSON,
			&r.SizeBytes, &r.CreatedAt, &r.LastUsedAt, &r.UseCount); err != nil {
			return nil, err
		}
		if lockJSON != "" {
			if err := json.Unmarshal([]byte(lockJSON), &r.LockfileHashes); err != nil {
				continue
			}
		}
		if inputsJSON != "" && inputsJSON != "{}" {
			if err := json.Unmarshal([]byte(inputsJSON), &r.Inputs); err != nil {
				continue
			}
		}
		// Commands must match exactly. A different migrate/seed run-
		// string or env would change which migrations get applied, so
		// the ancestor isn't a safe clone target.
		if r.LockfileHashes[CommandsHashKey] != currentCommandsHash {
			continue
		}
		if !vectorsAreAncestor(r.Inputs, currentInputs) {
			continue
		}
		total := vectorTotalLen(r.Inputs)
		// A candidate with the same total as current is an exact match
		// — that's the LookupSnapshot path. Strict prefix only.
		if total >= currentTotal {
			continue
		}
		if total > bestLen {
			bestCopy := r
			best = &bestCopy
			bestLen = total
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return best, nil
}

// vectorTotalLen sums the lengths of every input vector — the metric
// used to pick the "longest" ancestor when more than one qualifies.
func vectorTotalLen(m map[string]InputVector) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

// vectorsAreAncestor reports whether `cand` is a per-input prefix of
// `current`. Every input glob present in `cand` must also be present
// in `current` with the candidate's entries appearing at the same
// positions; `current` may carry additional entries at the end or
// additional input globs entirely (those represent the new files an
// incremental migrate would apply on top of the ancestor).
func vectorsAreAncestor(cand, current map[string]InputVector) bool {
	for glob, cVec := range cand {
		curVec, ok := current[glob]
		if !ok {
			// Candidate has an input the current run doesn't — it's
			// not a subset of current's state, so not an ancestor.
			return false
		}
		if len(cVec) > len(curVec) {
			return false
		}
		for i := range cVec {
			if cVec[i] != curVec[i] {
				return false
			}
		}
	}
	return true
}

// SnapshotEvictionCandidate is the slim view returned by
// `ListLRUEvictable`: just the fields the GC needs to drop the
// template DB + the row.
type SnapshotEvictionCandidate struct {
	Fingerprint  string
	Engine       string
	TemplateName string
	SourceDB     string
}

// ListLRUEvictable returns the snapshots above `cap` for a given
// repo, ordered by LRU (`last_used_at` ascending). Used by the
// inline GC fired after a fresh RecordSnapshot.
//
// `keep == 0` is treated as "no cap" and returns an empty slice
// (defense against a misconfigured config that would otherwise wipe
// every cached template).
func (s *Store) ListLRUEvictable(ctx context.Context, repoID int64, keep uint32) ([]SnapshotEvictionCandidate, error) {
	if keep == 0 || repoID == 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT fingerprint, engine, template_name, source_db
		FROM snapshots
		WHERE repo_id = ?
		ORDER BY last_used_at DESC
		LIMIT -1 OFFSET ?`, repoID, keep)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotEvictionCandidate
	for rows.Next() {
		var c SnapshotEvictionCandidate
		if err := rows.Scan(&c.Fingerprint, &c.Engine, &c.TemplateName, &c.SourceDB); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TouchSnapshot bumps `last_used_at` + `use_count` on a cache hit.
// Used by GC to keep an LRU ordering.
func (s *Store) TouchSnapshot(ctx context.Context, fingerprint string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE snapshots
		SET last_used_at = ?, use_count = use_count + 1
		WHERE fingerprint = ?`, nowMillis(), fingerprint)
	return err
}

// DeleteSnapshot drops a row by fingerprint. Called by GC after the
// underlying template DB is dropped from the engine.
func (s *Store) DeleteSnapshot(ctx context.Context, fingerprint string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM snapshots WHERE fingerprint = ?`, fingerprint)
	return err
}

// ListSnapshotsOlderThan returns every snapshot whose
// `last_used_at` is before `cutoffMillis`. Used by the
// max-age sweep.
func (s *Store) ListSnapshotsOlderThan(ctx context.Context, cutoffMillis int64) ([]SnapshotEvictionCandidate, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT fingerprint, engine, template_name, source_db
		FROM snapshots
		WHERE last_used_at < ?
		ORDER BY last_used_at ASC`, cutoffMillis)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotEvictionCandidate
	for rows.Next() {
		var c SnapshotEvictionCandidate
		if err := rows.Scan(&c.Fingerprint, &c.Engine, &c.TemplateName, &c.SourceDB); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SumSnapshotBytes returns COALESCE(SUM(size_bytes),0) across the
// template pool — used by the total-size sweep to decide whether
// eviction is needed.
func (s *Store) SumSnapshotBytes(ctx context.Context) (int64, error) {
	var sum int64
	row := s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes),0) FROM snapshots`)
	if err := row.Scan(&sum); err != nil {
		return 0, err
	}
	return sum, nil
}

// ListSnapshotsForRepo returns every snapshot belonging to the given
// repo. Returns an empty slice when repoID is 0 so callers can't
// accidentally drop snapshots that predate the repo_id column being
// populated. Used by MCP `snapshots_purge` to wipe a repo's cache.
func (s *Store) ListSnapshotsForRepo(ctx context.Context, repoID int64) ([]SnapshotEvictionCandidate, error) {
	if repoID == 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT fingerprint, engine, template_name, source_db
		FROM snapshots
		WHERE repo_id = ?
		ORDER BY last_used_at ASC`, repoID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotEvictionCandidate
	for rows.Next() {
		var c SnapshotEvictionCandidate
		if err := rows.Scan(&c.Fingerprint, &c.Engine, &c.TemplateName, &c.SourceDB); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListSnapshotsBeyondPerSource returns the snapshots that exceed `keep`
// per source, where a "source" is the migration-content key
// (`migrations_hash`) — templates that share migration content but
// differ in dump/lockfile/engine-version pile up under one source as a
// project's deps churn. Within each source the `keep` most-recently-used
// rows are retained; the rest (LRU) are returned for eviction.
//
// `keep == 0` is treated as "no limit" and returns an empty slice — a
// guard against a misconfigured 0 wiping every cached template (mirrors
// ListLRUEvictable's cap==0 guard).
//
// Implemented with a correlated count rather than a window function so
// it works regardless of the bundled SQLite version: a row is beyond the
// limit when at least `keep` siblings in the same source are strictly
// newer (ties broken deterministically by fingerprint).
func (s *Store) ListSnapshotsBeyondPerSource(ctx context.Context, keep uint32) ([]SnapshotEvictionCandidate, error) {
	if keep == 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT fingerprint, engine, template_name, source_db
		FROM snapshots s
		WHERE (
			SELECT COUNT(*) FROM snapshots s2
			WHERE s2.migrations_hash = s.migrations_hash
			  AND (s2.last_used_at > s.last_used_at
			       OR (s2.last_used_at = s.last_used_at AND s2.fingerprint > s.fingerprint))
		) >= ?
		ORDER BY migrations_hash, last_used_at ASC`, keep)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SnapshotEvictionCandidate
	for rows.Next() {
		var c SnapshotEvictionCandidate
		if err := rows.Scan(&c.Fingerprint, &c.Engine, &c.TemplateName, &c.SourceDB); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListSnapshotsLargestLRU returns every snapshot ordered by
// (size_bytes DESC, last_used_at ASC). The size-sweep iterates and
// drops from the top until total falls below the cap.
func (s *Store) ListSnapshotsLargestLRU(ctx context.Context) ([]SnapshotEvictionCandidate, []int64, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT fingerprint, engine, template_name, source_db, COALESCE(size_bytes,0)
		FROM snapshots
		ORDER BY COALESCE(size_bytes,0) DESC, last_used_at ASC`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var (
		cands []SnapshotEvictionCandidate
		sizes []int64
	)
	for rows.Next() {
		var c SnapshotEvictionCandidate
		var sz int64
		if err := rows.Scan(&c.Fingerprint, &c.Engine, &c.TemplateName, &c.SourceDB, &sz); err != nil {
			return nil, nil, err
		}
		cands = append(cands, c)
		sizes = append(sizes, sz)
	}
	return cands, sizes, rows.Err()
}
