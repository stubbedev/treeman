-- Hash cache optimizations.
--
-- 1. Secondary index on file_hashes(size, mtime_ns) enables
--    cross-worktree content-addressable reuse: a fresh worktree's
--    composer/Gemfile.lock with identical size+mtime to an existing
--    cached row in another worktree can skip the disk read.
--
-- 2. dir_hashes caches aggregate migration directory state keyed on
--    absolute directory path. mtime_ns bumps on add/rename/delete in
--    POSIX so a stable (mtime_ns, member_count) lets the framework
--    layer skip os.ReadDir + per-file SQLite lookups for the whole
--    directory. member_hash stores the rolled-up migrations_hash so
--    even multi-dir specs can reuse a per-dir contribution.

CREATE INDEX idx_file_hashes_content ON file_hashes(size, mtime_ns);

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
