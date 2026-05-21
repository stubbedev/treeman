-- 0003: persist the binlog replay position so the daemon resumes
-- after restart without re-applying events from the beginning of
-- the server's log retention window.
--
-- One row per (repo_id, source_db). The (filename, position)
-- columns are the legacy text protocol; (gtid_set) covers GTID
-- mode. Whichever the server uses, that column carries the cursor.

CREATE TABLE binlog_checkpoints (
    repo_id        INTEGER NOT NULL REFERENCES repos(id),
    source_db      TEXT NOT NULL,
    flavor         TEXT NOT NULL DEFAULT 'mysql',
    binlog_file    TEXT,
    binlog_pos     INTEGER,
    gtid_set       TEXT,
    last_event_ts  INTEGER,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (repo_id, source_db)
);
