-- Per-slug divergent snapshots: when `databases[].branch_scoped`
-- is true, `wt delete` captures the per-worktree DB's current
-- (diverged) state into a snapshot row keyed by (repo_id, slug,
-- engine, source_db). A later `wt create` for the same slug
-- restores from that snapshot instead of re-dumping the base
-- branch — so reopening a deleted worktree picks up where the
-- previous session left off.
--
-- The existing snapshots table keys cached templates by
-- `fingerprint` (a hash of the migration / lockfile inputs). That's
-- the "fresh from base + migrations" template — content shared
-- across worktrees with identical inputs. The per-slug snapshot is
-- a different beast: it holds user-modified data, scoped to one
-- worktree. We disambiguate the two via `slug_origin`:
--
--   NULL or ''  — the legacy fingerprint-keyed template (default).
--   'wt:<slug>' — a per-worktree divergent snapshot for that slug.
--
-- Lookup by slug uses the partial index below; lookup by
-- fingerprint (the original path) ignores rows with a non-empty
-- slug_origin so they don't pollute the LRU eviction.
ALTER TABLE snapshots ADD COLUMN slug_origin TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_snapshots_slug_origin
    ON snapshots(slug_origin)
    WHERE slug_origin <> '';
