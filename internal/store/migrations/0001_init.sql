-- Treeman SQLite schema. All timestamps are unix epoch milliseconds.
-- PRAGMA settings (journal_mode, synchronous, foreign_keys) are
-- applied via the connection-string DSN at pool open — they can't
-- run inside the migration transaction.
--
-- This file is the consolidated initial schema. Earlier installs
-- went through a chain of incremental migrations (0001 → 0010);
-- the runner records which versions a DB has already applied, so
-- existing databases keep their schema while fresh installs get
-- this single clean cut.

CREATE TABLE repos (
    id              INTEGER PRIMARY KEY,
    path            TEXT UNIQUE NOT NULL,
    name            TEXT NOT NULL,
    frameworks_json TEXT NOT NULL DEFAULT '[]',
    registered_at   INTEGER NOT NULL
);

CREATE TABLE worktrees (
    id                  INTEGER PRIMARY KEY,
    repo_id             INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    path                TEXT UNIQUE NOT NULL,
    slug                TEXT NOT NULL,
    branch              TEXT,
    created_at          INTEGER NOT NULL,
    deleted_at          INTEGER,
    last_visited_at     INTEGER,
    admin_dir           TEXT,
    inherited_env_json  TEXT
);
CREATE INDEX idx_worktrees_repo         ON worktrees(repo_id);
CREATE INDEX idx_worktrees_slug         ON worktrees(slug);
CREATE INDEX idx_worktrees_repo_visited ON worktrees(repo_id, last_visited_at DESC);
CREATE INDEX idx_worktrees_admin_dir    ON worktrees(admin_dir) WHERE admin_dir IS NOT NULL;

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
    use_count             INTEGER NOT NULL DEFAULT 0,
    repo_id               INTEGER REFERENCES repos(id)
);
CREATE INDEX idx_snapshots_lru      ON snapshots(source_db, last_used_at);
CREATE INDEX idx_snapshots_repo_lru ON snapshots(repo_id, last_used_at);

CREATE TABLE hook_runs (
    id           INTEGER PRIMARY KEY,
    worktree_id  INTEGER NOT NULL REFERENCES worktrees(id),
    phase        TEXT NOT NULL,
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER,
    exit_code    INTEGER,
    stdout_tail  TEXT,
    stderr_tail  TEXT,
    command      TEXT,
    group_idx    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_hook_runs_worktree ON hook_runs(worktree_id, started_at DESC);

-- File-content hash cache. Keyed by absolute path; (size, mtime_ns)
-- gate detects content changes without re-reading the file. The
-- secondary index on (size, mtime_ns) enables cross-worktree
-- content-addressable reuse: a fresh worktree's composer.lock /
-- Gemfile.lock with identical size+mtime to an existing cached row
-- can skip the disk read. Entries are advisory; stale rows get
-- overwritten on the next read.
CREATE TABLE file_hashes (
    path      TEXT PRIMARY KEY,
    size      INTEGER NOT NULL,
    mtime_ns  INTEGER NOT NULL,
    hash      TEXT NOT NULL,
    cached_at INTEGER NOT NULL
);
CREATE INDEX idx_file_hashes_content ON file_hashes(size, mtime_ns);

-- Aggregate migration-directory state keyed on absolute directory
-- path + spec_name + hash_mode. mtime_ns bumps on add/rename/delete
-- in POSIX so a stable (mtime_ns, member_count) lets the framework
-- layer skip os.ReadDir + per-file SQLite lookups for the whole
-- directory. member_hash stores the rolled-up migrations_hash so
-- multi-dir specs can reuse a per-dir contribution.
CREATE TABLE dir_hashes (
    dir          TEXT NOT NULL,
    spec_name    TEXT NOT NULL,
    hash_mode    TEXT NOT NULL,
    mtime_ns     INTEGER NOT NULL,
    member_count INTEGER NOT NULL,
    member_hash  TEXT NOT NULL,
    cached_at    INTEGER NOT NULL,
    PRIMARY KEY (dir, spec_name, hash_mode)
);