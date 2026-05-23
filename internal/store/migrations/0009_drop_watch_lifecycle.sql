-- 0009: drop the dead `repos.watch_lifecycle` column.
--
-- Lifecycle watcher is now always-on for every registered repo
-- (worktree add/remove is infrastructure, not user policy), so the
-- per-repo opt-in column added in 0007 has no remaining readers.
-- Requires SQLite 3.35+ for ALTER TABLE … DROP COLUMN; modernc.org/sqlite
-- ships well past that.

ALTER TABLE repos DROP COLUMN watch_lifecycle;
