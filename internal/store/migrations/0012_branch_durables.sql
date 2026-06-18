-- branch_durables tracks every branch_scoped DURABLE copy — the per-branch
-- preserved namespace a swap captures so switching back to a branch resumes
-- its data instead of re-seeding from the parent. Until now these were
-- deliberately untracked (see the 0006_active_branch_db note): their names
-- are a deterministic hash of (active namespace, branch), so the engine was
-- treated as the source of truth for "does branch X's copy exist".
--
-- That made them ungarbage-collectable. The hash is one-way, so a durable
-- captured at a since-deleted worktree path can never be re-derived, and the
-- only reaper (ReapBranchDurables) forward-computes names from CURRENTLY
-- registered worktrees crossed with a just-deleted branch. Durables for
-- branches deleted out-of-band, or whose worktree was removed, leaked forever
-- (observed: 511 orphan Elasticsearch indices exhausting the 3000-shard cap
-- on a single live worktree).
--
-- Recording each durable at capture time makes the whole pool enumerable, so
-- an orphan sweep can drop any durable whose branch no longer exists BY NAME
-- — no hash re-derivation, no dependence on the worktree still existing.
--
-- One row per (repo_id, durable_name). durable_name already encodes the
-- (active, branch) pair via its hash, so it is unique within a repo even when
-- several worktrees capture the same branch under different active prefixes.
--
-- worktree_id is ON DELETE SET NULL, not CASCADE: a durable MUST outlive its
-- worktree row (that is the whole point of "lives on"), and the orphan sweep
-- keys off branch existence, not worktree existence. CASCADE would re-open
-- the exact leak this table closes.
CREATE TABLE branch_durables (
    repo_id      INTEGER NOT NULL REFERENCES repos(id)     ON DELETE CASCADE,
    worktree_id  INTEGER          REFERENCES worktrees(id) ON DELETE SET NULL,
    engine       TEXT    NOT NULL,
    db_key       TEXT    NOT NULL,
    branch       TEXT    NOT NULL,
    durable_name TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,
    PRIMARY KEY (repo_id, durable_name)
);
CREATE INDEX idx_branch_durables_repo   ON branch_durables(repo_id);
CREATE INDEX idx_branch_durables_branch ON branch_durables(repo_id, branch);
