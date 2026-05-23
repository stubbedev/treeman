-- 0010: cache the user's shell environment per worktree.
--
-- The daemon's HEAD-watcher and file-watcher paths fire FinalizeWorktree
-- without an inheritedEnv (the CLI session that originally created the
-- worktree is long gone). Without the user's PATH, hook subprocesses can't
-- find their tools (composer, yarn, asdf-shimmed node, etc.). Cache the
-- env captured at `wt create` / `wt finalize` here so watcher reruns
-- rehydrate it.
--
-- Format: JSON object mapping env-var name → value. Stored as TEXT so
-- the SQLite driver doesn't need to know about JSON. NULL when the
-- worktree was created by a path that doesn't capture env (lifecycle
-- watcher's `git worktree add` outside the CLI).

ALTER TABLE worktrees ADD COLUMN inherited_env_json TEXT;
