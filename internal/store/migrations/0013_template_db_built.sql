-- template_db_built records, per template-path database of a worktree,
-- the input fingerprint at which that worktree's source + test-clone
-- namespaces were last successfully populated from the template.
--
-- A cache hit (snapshot row exists, fingerprint unchanged) used to still
-- drop + re-restore the source DB and every test clone from the
-- template — ~90s of pure I/O on a 16-clone repo that fired on every
-- finalize and every HEAD move. This table is the template-path analogue
-- of branch_db_migrated's gate: when the recorded fingerprint matches
-- the current one and the target namespaces still exist, the restore +
-- fan-out is skipped entirely (see cacheHitGeneric).
--
-- Keyed (worktree, db_key, engine): db_key is the rendered source DB
-- name (or key/index prefix for prefix-scoped engines). A row's absence
-- forces the restore (safe default), so clearing rows on reset /
-- recovery / teardown re-arms the restore.
CREATE TABLE template_db_built (
    worktree_id INTEGER NOT NULL REFERENCES worktrees(id) ON DELETE CASCADE,
    db_key      TEXT    NOT NULL,
    engine      TEXT    NOT NULL,
    fingerprint TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (worktree_id, db_key, engine)
);
