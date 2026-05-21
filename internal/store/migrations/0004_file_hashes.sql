-- File-content hash cache. Keyed by absolute path; (size, mtime_ns)
-- gate detects content changes without re-reading the file. Used by
-- prepare's snapshot-cache lookup to skip rehashing multi-GB dumps
-- + dense migration trees on every prepare run.
--
-- Entries are advisory: a stale row is detected on the next read
-- when size or mtime_ns disagrees, at which point the row gets
-- overwritten. No GC needed beyond `treeman daemon` periodically
-- pruning rows whose path no longer exists.

CREATE TABLE file_hashes (
    path      TEXT PRIMARY KEY,
    size      INTEGER NOT NULL,
    mtime_ns  INTEGER NOT NULL,
    hash      TEXT NOT NULL,
    cached_at INTEGER NOT NULL
);
