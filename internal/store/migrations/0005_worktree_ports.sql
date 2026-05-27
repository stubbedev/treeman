-- worktree_ports holds per-worktree TCP-port assignments for the
-- declarative slots under the top-level `ports:` block in
-- `.treeman.yaml`. One row per (worktree, slot name) — the same
-- worktree may own several slots (`octane`, `webpack`, `reverb`).
--
-- Two uniqueness constraints, both expressed as partial unique
-- indexes so a soft-deleted worktree row can release its ports
-- back into the pool without a row delete:
--
--   1. Each live worktree has at most one port per slot name.
--   2. Within one repo + slot, each port can be held by at most
--      one live worktree (avoids two worktrees claiming the same
--      TCP port for their octane server).
--
-- Allocations are released by deleting rows when the worktree is
-- deleted; the freed (repo_id, name, port) tuple is then available
-- to the next `wt create`.
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
