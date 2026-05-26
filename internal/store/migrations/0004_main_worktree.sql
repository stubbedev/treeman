-- Mark a worktree as the "main" working tree of its repo — the
-- repo root checkout, as opposed to a linked `.worktrees/<slug>`.
-- Lets the daemon enroll the main wt for the same watcher-driven
-- prepare/migrate/teardown lifecycle that linked wts already get.
--
-- Default 0 preserves every existing row's behaviour (linked wt).
ALTER TABLE worktrees ADD COLUMN is_main INTEGER NOT NULL DEFAULT 0;

-- At most one active main wt per repo. The partial index allows a
-- soft-deleted main row to coexist with a fresh enrollment, which
-- the EnsureMainWorktree resurrect path relies on.
CREATE UNIQUE INDEX idx_worktrees_one_main_per_repo
    ON worktrees(repo_id) WHERE is_main = 1 AND deleted_at IS NULL;
