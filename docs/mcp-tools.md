# MCP tool reference

[← back to README](../README.md)

Auto-generated from the MCP tool registry (`internal/mcp.Catalog`).
Run `just sync-docs` after adding or renaming a tool to refresh.
Tools marked **core** load up-front; the rest are revealed on demand
through the `tools` gateway (`action=list` / `action=enable`).

## branch

| Tool | Core | Summary |
|------|------|---------|
| `branch_scoped_status` |  | Inspect every branch_scoped DB — active namespace, occupying branch, resumable durable copies. |

## branches

| Tool | Core | Summary |
|------|------|---------|
| `branches_list` |  | List local + origin-only branches, annotated with which already occupy a worktree. |

## config

| Tool | Core | Summary |
|------|------|---------|
| `config_delete` |  | Delete a whole config FILE from disk. |
| `config_diff` |  | Preview the effect of a proposed config body — returns added/removed/changed dotted paths. |
| `config_get` |  | Read a config. |
| `config_history` |  | List stored generations of a config — every config_set/config_write/config_unset/config_delete/main-enable snapshots the prior content into SQLite (newest-first). |
| `config_locate` |  | List where every config treeman reads lives — the user-global config (~/. |
| `config_restore` |  | Restore a stored generation of a config (see config_history) back onto disk. |
| `config_schema` |  | Get the JSON Schema for treeman config (reflected from config. |
| `config_set` |  | Patch one config field by dotted path (e. |
| `config_unset` |  | Delete one key (or sequence element) from a config by dotted path (e. |
| `config_validate` |  | Confirm a config still parses + validates. |
| `config_write` |  | Overwrite a config with a full body. |

## connection

| Tool | Core | Summary |
|------|------|---------|
| `connection_probe` |  | Dry-test a connection string against an engine without writing it to . |

## daemon

| Tool | Core | Summary |
|------|------|---------|
| `daemon_control` |  | Control treemand. |
| `daemon_state` |  | Inspect treemand's runtime state — currently-running finalize/teardown goroutines (with age), watcher set (repo + per-worktree + lifecycle), per-repo sync backoff timers, last-skip reasons. |
| `daemon_status` |  | Check treemand: version, PID, watcher count. |

## db

| Tool | Core | Summary |
|------|------|---------|
| `db_dump` |  | Refresh the seed dump used for cold builds — runs mysqldump / pg_dump / mongodump / ES scroll-bulk. |
| `db_query` |  | Read OR write a configured engine directly — no shelling out to mysql/psql/mongosh/redis-cli. |
| `db_reset` |  | Reset a worktree's branch_scoped DBs to the base branch's data — drops the active namespace + current branch's durable copy, then re-seeds from the parent. |
| `db_save` |  | Capture a worktree's branch_scoped active namespaces into the CURRENT branch's durable copies without switching branches — a manual checkpoint of the swap lifecycle's capture half. |
| `db_schema_dump` |  | Dump the live schema for ONE database — mysql/postgres: CREATE TABLEs, mongo: collection list + samples, ES: index mapping, redis: SCAN-driven key summary. |

## doctor

| Tool | Core | Summary |
|------|------|---------|
| `doctor` | ✓ | Run treeman health checks — daemon, . |

## engine

| Tool | Core | Summary |
|------|------|---------|
| `engine_logs` |  | Tail the container logs for one configured engine — `docker logs --tail N --since S` (or podman/nerdctl/finch per the connection block's container_engine). |
| `engine_status` |  | Probe every engine in . |

## es

| Tool | Core | Summary |
|------|------|---------|
| `es_request` |  | Direct Elasticsearch REST passthrough using the configured ES connection (auth handled) — replaces curl for any endpoint db_query doesn't cover: _cat/indices, _cluster/health, _mapping, _settings, _aliases, doc get/index, _bulk, _search. |

## fw

| Tool | Core | Summary |
|------|------|---------|
| `fw_detect` |  | Detect migration + test frameworks in the repo. |

## hook

| Tool | Core | Summary |
|------|------|---------|
| `hook_log_read` |  | Read the FULL hook log file for one (worktree, phase, group_idx). |
| `hook_run` |  | Re-run one configured hook phase synchronously for a worktree. |

## init

| Tool | Core | Summary |
|------|------|---------|
| `init_repo` |  | Scaffold a fresh . |

## inputs

| Tool | Core | Summary |
|------|------|---------|
| `inputs_fingerprint` |  | Compute the per-database input-hash breakdown + check whether a matching snapshot exists. |

## logs

| Tool | Core | Summary |
|------|------|---------|
| `logs_hooks` |  | List recent hook_run rows for one worktree — command, exit code, stdout/stderr tails. |
| `logs_purge` |  | Delete event-log rows. |
| `logs_query` | ✓ | Query the SQLite event log — the PRIMARY diagnostic surface. |
| `logs_subscribe` |  | Live-stream events as they arrive via MCP progress notifications. |
| `logs_wait` |  | Block until min_count new events match the filter (or timeout). |

## main

| Tool | Core | Summary |
|------|------|---------|
| `main_worktree` |  | Manage main-worktree enrollment — opt the repo ROOT into the per-branch DB lifecycle. |

## notify

| Tool | Core | Summary |
|------|------|---------|
| `notify_test` |  | Send a test desktop notification through the configured (or auto-detected) backend to verify notify-send (Linux) / osascript (macOS) works before enabling notifications. |

## prepare

| Tool | Core | Summary |
|------|------|---------|
| `prepare_dry_run` |  | Render the prepare pipeline plan for a worktree WITHOUT executing — per-database: rendered source-db name, dump files that would be loaded, migrate + seed commands (with env), fanout count, expected fingerprint. |
| `prepare_run` | ✓ | Run the full prepare pipeline for a worktree (ensure source DB → dump → migrate → seed → snapshot → fanout clones). |

## prompts

| Tool | Core | Summary |
|------|------|---------|
| `prompts_list` |  | List every MCP prompt treeman registers — name, title, description, arguments, and a when-to-use trigger phrase. |

## registry

| Tool | Core | Summary |
|------|------|---------|
| `registry_register` |  | Add a WORKTREE row to SQLite without touching git. |
| `registry_repair` |  | Reconcile the SQLite worktree registry against `git worktree list` — registers what git knows that SQLite doesn't and marks deleted what SQLite knows that git doesn't. |
| `registry_unregister` |  | Mark a WORKTREE deleted in SQLite without touching git or external resources (DBs + on-disk path stay). |

## repo

| Tool | Core | Summary |
|------|------|---------|
| `repo_remove` |  | Drop a REPO from the registry (cascades to worktrees/events/snapshots/hook_runs). |

## schema

| Tool | Core | Summary |
|------|------|---------|
| `schema_install` |  | Generate the . |

## slug

| Tool | Core | Summary |
|------|------|---------|
| `slug_compute` |  | Preview the slug treeman will derive for a worktree path — which DB / redis prefix / index suffix the new worktree gets. |

## snapshot

| Tool | Core | Summary |
|------|------|---------|
| `snapshots_drop` |  | Evict ONE snapshot by fingerprint (engine-side template + SQLite row). |
| `snapshots_inspect` |  | Inspect ONE snapshot (by fingerprint, or engine+source_db) — SQLite row + does the engine-side template still exist + size + engine version. |
| `snapshots_list` |  | List cached snapshots (template DBs) for the repo. |
| `snapshots_prune` |  | Delete snapshot rows whose engine-side template no longer exists (orphans from out-of-band deletions or died captures). |
| `snapshots_purge` |  | DELETE every cached snapshot for a repo — forces every prepare to cold-build. |

## status

| Tool | Core | Summary |
|------|------|---------|
| `status_overview` | ✓ | Cross-repo fleet rollup: every registered worktree across every repo, bucketed stable/up(preparing)/down(teardown)/failed, derived from the latest lifecycle event per worktree. |

## sync

| Tool | Core | Summary |
|------|------|---------|
| `sync_now` |  | Force an immediate `git fetch --all --prune` + advance (ff or rebase per config) without waiting for the auto-fetch tick. |
| `sync_status` |  | Report per-repo + per-worktree git sync state: last-fetch time, fetch failures, next-retry, ahead/behind, dirty flag, last skip reason (dirty|no_upstream|non_ff|detached_head|rebase_conflict). |

## worktree

| Tool | Core | Summary |
|------|------|---------|
| `worktree_create` | ✓ | Create a new git worktree under . |
| `worktree_delete` |  | Tear down a worktree end-to-end: teardown hooks → drop DBs/redis prefixes/ES indices → remove the git worktree dir. |
| `worktree_finalize` |  | Re-run a worktree's setup hooks + prepare via the daemon (async — returns once queued, follow with logs_subscribe/logs_wait). |
| `worktree_list` | ✓ | List active worktrees (slug, branch, path) from the registry. |
| `worktree_repair` |  | Recover one stuck/broken worktree end-to-end: ensure registry row, ensure ports allocated, dispatch finalize via the daemon (or run prepare inline when the daemon is unreachable), and check each snapshot for orphan templates. |
| `worktree_show` |  | Show the full dossier for one worktree: slug, branch, path, ports, branch_scoped active-namespace, recent events. |

## prompts

Guided multi-step workflows (MCP prompts).

| Prompt | Summary |
|--------|---------|
| `bootstrap-new-repo` | repo has no .treeman.yaml yet; user wants treeman fully wired up from scratch |
| `cache-cleanup` | prepare keeps cold-building / cache seems wrong / snapshot rows look stale; safer than snapshots_purge |
| `diagnose-prepare-failure` | user says "why did prepare fail" / "what went wrong" / "the cold build is broken" |
| `edit-config` | user wants to change a treeman setting / "set the daemon log level" / "where does X config go, global or repo?" |
| `migration-trial` | user wants to validate a migration without committing to it / "is this migration safe?" |
| `scaffold-from-framework` | user wants a .treeman.yaml scaffolded but is willing to review before running prepare |
| `worktree-setup` | user says "set me up a worktree" / "make me a worktree for X" / "start working on branch Y" |

