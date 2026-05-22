-- 0007: lifecycle-hook watcher support.
--
-- `worktrees.admin_dir` caches the absolute path of the git-administrative
-- directory (`<common-dir>/worktrees/<name>/`) so the daemon can resolve
-- a worktree row from an fsnotify REMOVE event after the working tree is
-- already gone.
--
-- `repos.watch_lifecycle` is the per-repo opt-in for the watcher. The
-- daemon iterates only the repos where this is 1 (and the config bool is
-- also true) when subscribing to `<common-dir>/worktrees/`.

ALTER TABLE worktrees ADD COLUMN admin_dir TEXT;
CREATE INDEX idx_worktrees_admin_dir ON worktrees(admin_dir) WHERE admin_dir IS NOT NULL;

ALTER TABLE repos ADD COLUMN watch_lifecycle INTEGER NOT NULL DEFAULT 0;
