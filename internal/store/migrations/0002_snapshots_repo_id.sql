-- 0002: associate snapshots with a repo so the inline GC can cap
-- the cache per repo. Existing rows (if any) get repo_id = NULL;
-- the GC just skips those — they age out via the periodic sweep on
-- KeepPerSource / MaxAgeDays / MaxTotalGb.

ALTER TABLE snapshots ADD COLUMN repo_id INTEGER REFERENCES repos(id);

CREATE INDEX idx_snapshots_repo_lru ON snapshots(repo_id, last_used_at);
