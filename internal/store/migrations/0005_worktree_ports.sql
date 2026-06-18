-- worktree_ports holds per-worktree TCP-port assignments for the
-- declarative slots under the top-level `ports:` block in
-- `.treeman.yaml`. One row per (worktree, slot name) — the same
-- worktree may own several slots (`octane`, `webpack`, `reverb`).
--
-- Two uniqueness constraints:
--
--   1. Each worktree has at most one port per slot name.
--   2. Within one repo + slot, each port is held by at most one
--      worktree (avoids two worktrees claiming the same TCP port
--      for their octane server).
--
-- Both are plain (non-partial) unique indexes, so they apply to
-- EVERY row, including rows whose worktree has been soft-deleted.
-- A reservation is therefore released only by physically DELETEing
-- its row (store.ReleaseWorktreePorts) — soft-deleting the worktree
-- is not enough. ListUsedPorts would skip a soft-deleted row when
-- scanning for free ports, but index (2) would still reject the
-- re-insert, so the freed (repo_id, name, port) tuple stays
-- unusable until the row is gone. Every teardown path (CLI inline,
-- daemon TeardownWorktree, lifecycle teardownOrphan) must call
-- ReleaseWorktreePorts before MarkWorktreeDeleted.
CREATE TABLE worktree_ports (
    id          INTEGER PRIMARY KEY,
    repo_id     INTEGER NOT NULL REFERENCES repos(id)     ON DELETE CASCADE,
    worktree_id INTEGER NOT NULL REFERENCES worktrees(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    port        INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    allocated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_worktree_ports_one_per_slot
    ON worktree_ports(worktree_id, name);
CREATE UNIQUE INDEX idx_worktree_ports_one_per_port
    ON worktree_ports(repo_id, name, port);
CREATE INDEX idx_worktree_ports_repo ON worktree_ports(repo_id);
