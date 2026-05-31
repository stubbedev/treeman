-- branch_db_migrated records, per branch-scoped database AND branch, the
-- input fingerprint (migrations + lockfile + commands + engine version)
-- at which that branch's durable copy was last successfully migrated.
--
-- The swap lifecycle re-runs `migrate` on every prepare. When a branch is
-- resumed from its durable copy and the migration inputs are byte-for-byte
-- what they were the last time migrate ran for that branch, the durable
-- copy is already at head — re-running migrate is pure cost. This table
-- lets prepare skip that redundant migrate (see runBranchScoped).
--
-- Keyed (worktree, db_key, branch): db_key is the rendered active
-- namespace (stable across branch switches), branch is whose data the
-- fingerprint describes. A row's mere absence forces a migrate (safe
-- default), so dropping rows on reset/teardown re-arms the migrate.
CREATE TABLE branch_db_migrated (
    worktree_id INTEGER NOT NULL REFERENCES worktrees(id) ON DELETE CASCADE,
    db_key      TEXT    NOT NULL,
    branch      TEXT    NOT NULL,
    fingerprint TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (worktree_id, db_key, branch)
);
