# RPC reference

[← back to docs](README.md)

Auto-generated from the `Method*` / `Kind*` / `Task*` / `Param*`
constants in `internal/rpc/rpc.go`. Run `just sync-docs` after touching
the RPC surface to refresh.

## Methods

RPC methods the daemon answers. Wire envelope: `{"method": <m>, "<m>": {…args}}` (protocol v2).

| Value | Constant | Notes |
|-------|----------|-------|
| `status` | `rpc.MethodStatus` |  |
| `ping` | `rpc.MethodPing` |  |
| `repo_register` | `rpc.MethodRepoRegister` |  |
| `watcher_start` | `rpc.MethodWatcherStart` |  |
| `watcher_stop` | `rpc.MethodWatcherStop` |  |
| `watcher_list` | `rpc.MethodWatcherList` |  |
| `worktree_list` | `rpc.MethodWorktreeList` |  |
| `config_reload` | `rpc.MethodConfigReload` |  |
| `repo_remove` | `rpc.MethodRepoRemove` |  |
| `shutdown` | `rpc.MethodShutdown` |  |
| `sync_now` | `rpc.MethodSyncNow` |  |
| `sync_status` | `rpc.MethodSyncStatus` |  |
| `daemon_state` | `rpc.MethodDaemonState` |  |
| `run_plan` | `rpc.MethodRunPlan` |  |
| `event_subscribe` | `rpc.MethodEventSubscribe` |  |

## Response kinds

The `kind` field on every response.

| Value | Constant | Notes |
|-------|----------|-------|
| `ok` | `rpc.KindOk` |  |
| `pong` | `rpc.KindPong` |  |
| `status` | `rpc.KindStatus` |  |
| `repo_registered` | `rpc.KindRepoRegistered` |  |
| `watcher_started` | `rpc.KindWatcherStarted` |  |
| `watcher_stopped` | `rpc.KindWatcherStopped` |  |
| `watcher_list` | `rpc.KindWatcherList` |  |
| `worktree_list` | `rpc.KindWorktreeList` |  |
| `plan_queued` | `rpc.KindPlanQueued` |  |
| `plan_result` | `rpc.KindPlanResult` |  |
| `sync_result` | `rpc.KindSyncResult` |  |
| `sync_status` | `rpc.KindSyncStatus` |  |
| `daemon_state` | `rpc.KindDaemonState` |  |
| `event` | `rpc.KindEvent` |  |
| `error` | `rpc.KindError` |  |

## Plan tasks

State mutations the daemon performs; submitted as a plan via the `run_plan` method.

| Value | Constant | Notes |
|-------|----------|-------|
| `prepare` | `rpc.TaskPrepare` | prepare.Run |
| `db_reset` | `rpc.TaskDBReset` | drop branch-scoped + re-prepare |
| `db_save` | `rpc.TaskDBSave` | capture branch-scoped → durable copy |
| `hook_run` | `rpc.TaskHookRun` | run one hook phase |
| `snapshots_purge` | `rpc.TaskSnapshotsPurge` | drop every cached template DB |
| `main_purge_dbs` | `rpc.TaskMainPurgeDBs` | drop main_<branch> DBs across branches |
| `worktree_register` | `rpc.TaskWorktreeRegister` | EnsureRepo + EnsureWorktree row |
| `worktree_unregister` | `rpc.TaskWorktreeUnregister` | mark a worktree row deleted |
| `logs_purge` | `rpc.TaskLogsPurge` | delete event-log rows by filter |
| `registry_repair` | `rpc.TaskRegistryRepair` | reconcile SQLite vs git worktree list |
| `config_write` | `rpc.TaskConfigWrite` | snapshot + atomic-write .treeman.yaml + reload |
| `worktree_create` | `rpc.TaskWorktreeCreate` | git add + register + ports (+ async finalize) |
| `worktree_finalize` | `rpc.TaskWorktreeFinalize` | setup hooks + prepare tail |
| `worktree_teardown` | `rpc.TaskWorktreeTeardown` | teardown hooks + DB drop + git remove |

## Task params

String-keyed side-channel on a Task (booleans encoded as `"1"`).

| Value | Constant | Notes |
|-------|----------|-------|
| `branch` | `rpc.ParamBranch` |  |
| `from` | `rpc.ParamFrom` |  |
| `path` | `rpc.ParamPath` |  |
| `phase` | `rpc.ParamPhase` |  |
| `force` | `rpc.ParamForce` |  |
| `body` | `rpc.ParamBody` |  |
| `engine_filter` | `rpc.ParamEngineFilter` |  |
| `no_fetch` | `rpc.ParamNoFetch` |  |
| `skip_hooks` | `rpc.ParamSkipHooks` |  |
| `skip_prepare` | `rpc.ParamSkipPrepare` |  |
| `levels` | `rpc.ParamLevels` |  |
| `event_types` | `rpc.ParamEventTypes` |  |
| `until_ms` | `rpc.ParamUntilMs` |  |
| `repo` | `rpc.ParamRepo` |  |
| `worktree` | `rpc.ParamWorktree` |  |

