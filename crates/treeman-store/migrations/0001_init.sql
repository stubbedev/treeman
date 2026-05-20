-- Treeman SQLite schema. All timestamps are unix epoch milliseconds.
-- PRAGMA settings (journal_mode, synchronous, foreign_keys) are applied via
-- `SqliteConnectOptions` at pool open — they can't run inside the migration
-- transaction.

CREATE TABLE repos (
    id              INTEGER PRIMARY KEY,
    path            TEXT UNIQUE NOT NULL,
    name            TEXT NOT NULL,
    frameworks_json TEXT NOT NULL DEFAULT '[]',
    registered_at   INTEGER NOT NULL
);

CREATE TABLE worktrees (
    id          INTEGER PRIMARY KEY,
    repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    path        TEXT UNIQUE NOT NULL,
    slug        TEXT NOT NULL,
    branch      TEXT,
    created_at  INTEGER NOT NULL,
    deleted_at  INTEGER
);
CREATE INDEX idx_worktrees_repo ON worktrees(repo_id);
CREATE INDEX idx_worktrees_slug ON worktrees(slug);

CREATE TABLE events (
    id           INTEGER PRIMARY KEY,
    ts           INTEGER NOT NULL,
    level        TEXT NOT NULL CHECK (level IN ('debug','info','warn','error')),
    repo_id      INTEGER REFERENCES repos(id),
    worktree_id  INTEGER REFERENCES worktrees(id),
    event_type   TEXT NOT NULL,
    phase        TEXT,
    message      TEXT,
    payload_json TEXT NOT NULL DEFAULT '{}',
    duration_ms  INTEGER
);
CREATE INDEX idx_events_ts       ON events(ts DESC);
CREATE INDEX idx_events_worktree ON events(worktree_id, ts DESC);
CREATE INDEX idx_events_type     ON events(event_type, ts DESC);

CREATE TABLE snapshots (
    fingerprint           TEXT PRIMARY KEY,
    engine                TEXT NOT NULL,
    engine_version        TEXT NOT NULL,
    source_db             TEXT NOT NULL,
    template_name         TEXT NOT NULL,
    migrations_hash       TEXT NOT NULL,
    dump_hash             TEXT,
    lockfile_hashes_json  TEXT NOT NULL DEFAULT '{}',
    size_bytes            INTEGER,
    created_at            INTEGER NOT NULL,
    last_used_at          INTEGER NOT NULL,
    use_count             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_snapshots_lru ON snapshots(source_db, last_used_at);

CREATE TABLE hook_runs (
    id           INTEGER PRIMARY KEY,
    worktree_id  INTEGER NOT NULL REFERENCES worktrees(id),
    phase        TEXT NOT NULL,
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER,
    exit_code    INTEGER,
    stdout_tail  TEXT,
    stderr_tail  TEXT
);
CREATE INDEX idx_hook_runs_worktree ON hook_runs(worktree_id, started_at DESC);
