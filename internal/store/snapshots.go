package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	_ = json.Unmarshal([]byte(lockJSON), &r.LockfileHashes)
	return &r, nil
}

// RecordSnapshot inserts (or updates on conflict) the snapshots row
// for a freshly built template. `sizeBytes` may be 0 if unknown.
func (s *Store) RecordSnapshot(ctx context.Context, r SnapshotRecord) error {
	if r.LockfileHashes == nil {
		r.LockfileHashes = map[string]string{}
	}
	lockJSON, _ := json.Marshal(r.LockfileHashes)
	now := nowMillis()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	if r.LastUsedAt == 0 {
		r.LastUsedAt = now
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO snapshots(fingerprint, engine, engine_version, source_db,
		                      template_name, migrations_hash, dump_hash,
		                      lockfile_hashes_json, size_bytes, created_at,
		                      last_used_at, use_count)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?,''), ?, NULLIF(?,0), ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
		    template_name        = excluded.template_name,
		    engine_version       = excluded.engine_version,
		    source_db            = excluded.source_db,
		    migrations_hash      = excluded.migrations_hash,
		    dump_hash            = excluded.dump_hash,
		    lockfile_hashes_json = excluded.lockfile_hashes_json,
		    size_bytes           = excluded.size_bytes,
		    last_used_at         = excluded.last_used_at`,
		r.Fingerprint, r.Engine, r.EngineVersion, r.SourceDB,
		r.TemplateName, r.MigrationsHash, r.DumpHash, string(lockJSON),
		r.SizeBytes, r.CreatedAt, r.LastUsedAt, r.UseCount)
	return err
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
