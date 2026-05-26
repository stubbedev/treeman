-- macOS APFS is case-insensitive by default — a user who creates a
-- worktree at `/Users/jane/Repo` and later runs `wt show` from
-- `/Users/jane/repo` (different case typed in the shell) must hit
-- the same row. Queries against `worktrees.path` and `repos.path`
-- now use `COLLATE NOCASE` for the comparison; these expression
-- indexes keep the lookups index-backed instead of full-scan.
--
-- The columns themselves stay BINARY so the original case the user
-- typed during `wt create` round-trips intact in `wt list` /
-- `logs hooks`. Only the comparison folds case.
CREATE INDEX IF NOT EXISTS idx_worktrees_path_nocase ON worktrees(path COLLATE NOCASE);
CREATE INDEX IF NOT EXISTS idx_repos_path_nocase     ON repos(path COLLATE NOCASE);
