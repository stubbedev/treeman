-- Per-repo history of overwritten config files (`.treeman.yaml`).
--
-- Replaces the old `<path>.bak.<timestamp>` files that `config_set` /
-- `config_write` used to drop in the project root. Every overwrite now
-- snapshots the PREVIOUS on-disk content here, keyed by repo, so the
-- working tree stays clean and `treeman config history|restore` can walk
-- the chain. `generation` is a per-(repo,path) monotonic counter used as
-- the stable handle in the CLI; `content` is the raw bytes as they were
-- before the write. No rotation cap — SQLite rows are cheap and the
-- history is worth keeping.
CREATE TABLE config_generations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    path       TEXT    NOT NULL,
    generation INTEGER NOT NULL,
    content    BLOB    NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_config_generations_repo
    ON config_generations(repo_id, path, generation DESC);
