package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

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
	SizeBytes      int64
	CreatedAt      int64
	LastUsedAt     int64
	UseCount       int64
	RepoID         int64
}

// LookupSnapshot returns the snapshot row for `fingerprint`, or
// (nil, nil) if no row exists. Errors only on DB faults.
func (s *Store) LookupSnapshot(ctx context.Context, fingerprint string) (*SnapshotRecord, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT fingerprint, engine, engine_version, source_db, template_name,
		       migrations_hash, COALESCE(dump_hash,''), COALESCE(lockfile_hashes_json,'{}'),
		       COALESCE(size_bytes,0), created_at, last_used_at, use_count
		FROM snapshots WHERE fingerprint = ?`, fingerprint)
	var r SnapshotRecord
	var lockJSON string
	err := row.Scan(&r.Fingerprint, &r.Engine, &r.EngineVersion, &r.SourceDB,
		&r.TemplateName, &r.MigrationsHash, &r.DumpHash, &lockJSON,
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
	return &r, nil
}

// RecordSnapshot inserts (or updates on conflict) the snapshots row
// for a freshly built template. `sizeBytes` may be 0 if unknown.
func (s *Store) RecordSnapshot(ctx context.Context, r SnapshotRecord) error {
	if r.LockfileHashes == nil {
		r.LockfileHashes = map[string]string{}
	}
	lockJSON, err := json.Marshal(r.LockfileHashes)
	if err != nil {
		return fmt.Errorf("encode lockfile_hashes: %w", err)
	}
	now := nowMillis()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.LastUsedAt == 0 {
		r.LastUsedAt = now
	}
	var repoID interface{}
	if r.RepoID > 0 {
		repoID = r.RepoID
	}
	_, err = s.DB.ExecContext(ctx, `
		INSERT INTO snapshots(fingerprint, engine, engine_version, source_db,
		                      template_name, migrations_hash, dump_hash,
		                      lockfile_hashes_json, size_bytes, created_at,
		                      last_used_at, use_count, repo_id)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?,''), ?, NULLIF(?,0), ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
		    template_name        = excluded.template_name,
		    engine_version       = excluded.engine_version,
		    source_db            = excluded.source_db,
		    migrations_hash      = excluded.migrations_hash,
		    dump_hash            = excluded.dump_hash,
		    lockfile_hashes_json = excluded.lockfile_hashes_json,
		    size_bytes           = excluded.size_bytes,
		    last_used_at         = excluded.last_used_at,
		    repo_id              = excluded.repo_id`,
		r.Fingerprint, r.Engine, r.EngineVersion, r.SourceDB,
		r.TemplateName, r.MigrationsHash, r.DumpHash, string(lockJSON),
		r.SizeBytes, r.CreatedAt, r.LastUsedAt, r.UseCount, repoID)
	return err
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
// `cap == 0` is treated as "no cap" and returns an empty slice
// (defense against a misconfigured config that would otherwise wipe
// every cached template).
func (s *Store) ListLRUEvictable(ctx context.Context, repoID int64, cap uint32) ([]SnapshotEvictionCandidate, error) {
	if cap == 0 || repoID == 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT fingerprint, engine, template_name, source_db
		FROM snapshots
		WHERE repo_id = ?
		ORDER BY last_used_at DESC
		LIMIT -1 OFFSET ?`, repoID, cap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	defer rows.Close()
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
// table — used by the total-size sweep to decide whether eviction
// is needed.
func (s *Store) SumSnapshotBytes(ctx context.Context) (int64, error) {
	var sum int64
	row := s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM snapshots`)
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
	defer rows.Close()
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
	defer rows.Close()
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
