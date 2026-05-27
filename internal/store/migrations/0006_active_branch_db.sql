-- active_branch_db records, per branch-scoped database, WHICH branch's
-- data currently occupies the ACTIVE namespace (the DB name / key
-- prefix the app connects to). It is the restart-safe source of truth
-- the swap lifecycle reads on a HEAD change: knowing the OLD branch
-- lets treeman capture the active namespace into that branch's durable
-- copy BEFORE overwriting it with the NEW branch's data.
--
-- The HEAD watcher's in-memory previous-ref doesn't survive a daemon
-- restart, so this marker — not the watcher — drives the swap.
--
-- One row per (worktree, db_key). db_key is the rendered active
-- namespace, which is stable across branch switches (it keys off the
-- worktree's path, not the branch), so it uniquely identifies the
-- database within a worktree even as `branch` swaps underneath it.
--
-- Durable per-branch copies themselves are NOT tracked here: their
-- names are deterministic (a hash of active namespace + branch), so the
-- engine itself is the source of truth for "does branch X's copy
-- exist". This table only answers "what's in the active slot now".
CREATE TABLE active_branch_db (
    repo_id     INTEGER NOT NULL REFERENCES repos(id)     ON DELETE CASCADE,
    worktree_id INTEGER NOT NULL REFERENCES worktrees(id) ON DELETE CASCADE,
    db_key      TEXT    NOT NULL,
    branch      TEXT    NOT NULL,
    engine      TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (worktree_id, db_key)
);
CREATE INDEX idx_active_branch_db_repo ON active_branch_db(repo_id);
