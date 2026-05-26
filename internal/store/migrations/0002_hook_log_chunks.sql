-- Persistent stdout+stderr capture for hook_runs. Each chunk is one
-- buffered flush from the hook runner's piped reader goroutine; rows
-- are append-only and ordered by id. Raw bytes are kept verbatim so
-- ANSI escape codes (yarn / npm / composer color) round-trip through
-- `treeman logs hooks show <id>` without re-encoding.
--
-- ON DELETE CASCADE keeps chunks tied to their hook_runs row — when
-- the daemon prune job (driven by logs.keep_days) expires a hook_run,
-- its chunks vanish with it.
CREATE TABLE hook_log_chunks (
    id          INTEGER PRIMARY KEY,
    hook_run_id INTEGER NOT NULL REFERENCES hook_runs(id) ON DELETE CASCADE,
    ts          INTEGER NOT NULL,
    stream      TEXT NOT NULL CHECK (stream IN ('stdout', 'stderr', 'merged')),
    body        BLOB NOT NULL
);

CREATE INDEX idx_hook_log_chunks_run ON hook_log_chunks(hook_run_id, id);
CREATE INDEX idx_hook_log_chunks_ts  ON hook_log_chunks(ts);
