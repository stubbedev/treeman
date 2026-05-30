-- Index the snapshot GC's per-source sweep. ListSnapshotsBeyondPerSource
-- runs a correlated subquery that, for each row, counts siblings sharing
-- the same migrations_hash with a newer (last_used_at, fingerprint). With
-- only idx_snapshots_lru(source_db, …) and idx_snapshots_repo_lru(repo_id,
-- …) available, that subquery degraded to a full table scan per row —
-- O(n²) over the template pool on every eviction sweep. This covering
-- index lets the subquery resolve as an index range scan.
CREATE INDEX IF NOT EXISTS idx_snapshots_migrations
    ON snapshots(migrations_hash, last_used_at, fingerprint);
