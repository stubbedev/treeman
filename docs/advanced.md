# Advanced — snapshots, frameworks

[← back to README](../README.md)

## Snapshot cache + GC

Each `prepare` run fingerprints the **content** that determines a
template DB — the engine + engine version, the dump's content hash, and
the content hashes (BLAKE3) of the migration files and lockfiles — into
a single key. It deliberately excludes the source-DB name and the
detected framework, so one cached template is reused across every
worktree and branch whose inputs match. If a row with that key exists
in the SQLite `snapshots` table AND the template DB still exists on the
engine, treeman skips the cold rebuild and `CREATE DATABASE … TEMPLATE`
/ `INSERT … SELECT`s into the test clones directly.

Cache-hit vs cold-build is derived purely from those hashes — there is
no `on: rebuild` knob. To force a rebuild, change an input.

### Retention / GC

A periodic daemon sweep (every `snapshots.gc_interval_minutes`, default
60) evicts cached templates that exceed any of the retention knobs, then
drops the engine-side template DB + its SQLite row. Each eviction emits
an event so you can see what went and why:

| Knob | Default | Evicts when… | Event |
|---|---|---|---|
| `snapshots.cap_per_repo` | 8 | a repo has more than N cached templates (LRU) | `snapshots:evict:cap` |
| `snapshots.keep_per_source` | 500 | a single source DB has more than N templates | `snapshots:evict:source` |
| `snapshots.max_age_days` | 30 | a template is older than N days | `snapshots:evict:age` |
| `snapshots.max_total_gb` | 50 | total cached size exceeds N GB (largest-first) | `snapshots:evict:size` |

Eviction also runs inline right after a new template is recorded (the
`cap_per_repo` check), so disk stays bounded without waiting for the
sweep. See the full event list in [events.md](events.md).

## Frameworks

`treeman fw detect` lists every framework treeman recognises in the
current repo, and you can declare your own under the `frameworks:`
config block. The full built-in preset table — markers, migration
directories, and engine hint per framework — is generated from the
detector registry: **[frameworks.md](frameworks.md)**.

All migration files, lockfiles, and dumps are content-hashed (BLAKE3);
there is no per-framework "hash mode" (the legacy filename/checksum
distinction is gone — it relied on an append-only convention that
wasn't enforced, so an in-place edit could keep a stale template
alive). Per-directory hashes are cached against the dir's `(mtime, file
count)` so an unchanged tree skips the re-read; any content, add, or
remove moves the fingerprint and triggers a rebuild.
