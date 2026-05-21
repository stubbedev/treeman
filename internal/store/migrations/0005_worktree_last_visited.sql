-- 0005: per-worktree last_visited_at + activity timestamp so the CLI
-- can sort `wt list` by recency and surface the previous worktree
-- (`wt prev`) across shell sessions. Existing rows seed to created_at
-- via UPDATE; the column itself stays NULL-tolerant for backfills.

ALTER TABLE worktrees ADD COLUMN last_visited_at INTEGER;

UPDATE worktrees SET last_visited_at = created_at WHERE last_visited_at IS NULL;

CREATE INDEX idx_worktrees_repo_visited ON worktrees(repo_id, last_visited_at DESC);
