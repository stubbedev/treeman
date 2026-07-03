# Event reference

[← back to README](../README.md)

Auto-generated from `internal/store/eventtypes.go` (the `store.Evt*`
constants — the single source of truth for every `event_type` written
to the SQLite log). Run `just sync-docs` after adding an event to refresh.

Filter the log on these with `treeman logs tail --event-type <value>` or
the `logs_query` MCP tool. Naming scheme: `domain:object:stage` — the
`domain` is the config option the event relates to or the subsystem
that emits it.

## worktrees.copies / worktrees.links — bring-in passes

| Event | Constant |
|-------|----------|
| `copies:start` | `store.EvtCopiesStart` |
| `copies:path` | `store.EvtCopiesPath` |
| `copies:end` | `store.EvtCopiesEnd` |
| `links:start` | `store.EvtLinksStart` |
| `links:path` | `store.EvtLinksPath` |
| `links:end` | `store.EvtLinksEnd` |

## ports

| Event | Constant |
|-------|----------|
| `ports:allocate` | `store.EvtPortsAllocate` |
| `ports:error` | `store.EvtPortsError` |

## patches

| Event | Constant |
|-------|----------|
| `patches:apply` | `store.EvtPatchesApply` |

## hooks

| Event | Constant |
|-------|----------|
| `hooks:start` | `store.EvtHooksStart` |
| `hooks:end` | `store.EvtHooksEnd` |

## databases — prepare pipeline

| Event | Constant |
|-------|----------|
| `prepare:start` | `store.EvtPrepareStart` |
| `prepare:phase` | `store.EvtPreparePhase` |
| `prepare:end` | `store.EvtPrepareEnd` |
| `prepare:error` | `store.EvtPrepareError` |
| `prepare:skip` | `store.EvtPrepareSkip` |
| `prepare:incremental:start` | `store.EvtPrepareIncrementalStart` |
| `prepare:incremental:fallback` | `store.EvtPrepareIncrementalFallback` |
| `prepare:dumponly:start` | `store.EvtPrepareDumpOnlyStart` |
| `prepare:dumponly:fallback` | `store.EvtPrepareDumpOnlyFallback` |
| `prepare:rollback:start` | `store.EvtPrepareRollbackStart` |
| `prepare:rollback:fallback` | `store.EvtPrepareRollbackFallback` |
| `prepare:unsupported` | `store.EvtPrepareUnsupported` |

## databases — db operations

| Event | Constant |
|-------|----------|
| `db:drop` | `store.EvtDBDrop` |
| `db:reset` | `store.EvtDBReset` |
| `db:save` | `store.EvtDBSave` |
| `db:teardown:skip` | `store.EvtDBTeardownSkip` |
| `db:teardown:error` | `store.EvtDBTeardownError` |

## databases[].migrate

| Event | Constant |
|-------|----------|
| `migrate:skip` | `store.EvtMigrateSkip` |

## databases[].test_clones — fanout + per-clone restore

| Event | Constant |
|-------|----------|
| `clones:start` | `store.EvtClonesStart` |
| `clones:end` | `store.EvtClonesEnd` |
| `clones:restore:end` | `store.EvtClonesRestoreEnd` |
| `clones:restore:error` | `store.EvtClonesRestoreError` |

## snapshots

| Event | Constant |
|-------|----------|
| `snapshots:cache:hit` | `store.EvtSnapshotsCacheHit` |
| `snapshots:cache:miss` | `store.EvtSnapshotsCacheMiss` |
| `snapshots:cache:fallback` | `store.EvtSnapshotsCacheFallback` |
| `snapshots:strategy` | `store.EvtSnapshotsStrategy` |
| `snapshots:evict:cap` | `store.EvtSnapshotsEvictCap` |
| `snapshots:evict:source` | `store.EvtSnapshotsEvictSource` |
| `snapshots:evict:age` | `store.EvtSnapshotsEvictAge` |
| `snapshots:evict:size` | `store.EvtSnapshotsEvictSize` |
| `snapshots:purge` | `store.EvtSnapshotsPurge` |
| `snapshots:purge:end` | `store.EvtSnapshotsPurgeEnd` |
| `snapshots:prewarm` | `store.EvtSnapshotsPrewarm` |
| `snapshots:prewarm:claim` | `store.EvtSnapshotsPrewarmClaim` |

## auto_fetch — fetch + branch maintenance

| Event | Constant |
|-------|----------|
| `fetch:pull` | `store.EvtFetchPull` |
| `fetch:skip` | `store.EvtFetchSkip` |
| `fetch:error` | `store.EvtFetchError` |
| `fetch:rebase:error` | `store.EvtFetchRebaseError` |
| `branch:prune` | `store.EvtBranchPrune` |
| `branch:reap` | `store.EvtBranchReap` |
| `branch:capture:skip` | `store.EvtBranchCaptureSkip` |
| `branch:capture:error` | `store.EvtBranchCaptureError` |

## databases[].inputs + HEAD — watcher

| Event | Constant |
|-------|----------|
| `watch:start` | `store.EvtWatchStart` |
| `watch:input` | `store.EvtWatchInput` |
| `watch:head` | `store.EvtWatchHead` |

## main_worktree

| Event | Constant |
|-------|----------|
| `main:enroll` | `store.EvtMainEnroll` |
| `main:disable` | `store.EvtMainDisable` |
| `main:purge` | `store.EvtMainPurge` |

## worktree lifecycle (no config key)

| Event | Constant |
|-------|----------|
| `worktree:create:start` | `store.EvtWorktreeCreateStart` |
| `worktree:create:checkout` | `store.EvtWorktreeCreateCheckout` |
| `worktree:create:end` | `store.EvtWorktreeCreateEnd` |
| `worktree:create:error` | `store.EvtWorktreeCreateError` |
| `worktree:create:cancel` | `store.EvtWorktreeCreateCancel` |
| `worktree:create:deferred` | `store.EvtWorktreeCreateDeferred` |
| `worktree:delete:start` | `store.EvtWorktreeDeleteStart` |
| `worktree:delete:end` | `store.EvtWorktreeDeleteEnd` |
| `worktree:delete:error` | `store.EvtWorktreeDeleteError` |
| `worktree:reap:start` | `store.EvtWorktreeReapStart` |
| `worktree:reap:end` | `store.EvtWorktreeReapEnd` |
| `worktree:recover:drop` | `store.EvtWorktreeRecoverDrop` |
| `worktree:recover:error` | `store.EvtWorktreeRecoverError` |

## daemon / global

| Event | Constant |
|-------|----------|
| `daemon:start` | `store.EvtDaemonStart` |
| `daemon:stop` | `store.EvtDaemonStop` |
| `config:reload` | `store.EvtConfigReload` |
| `registry:remove` | `store.EvtRegistryRemove` |
| `plan:start` | `store.EvtPlanStart` |
| `plan:end` | `store.EvtPlanEnd` |
| `plan:error` | `store.EvtPlanError` |

## MCP audit events — one per mutating MCP tool. Explicit constants

| Event | Constant |
|-------|----------|
| `mcp:config:set` | `store.EvtMCPConfigSet` |
| `mcp:config:unset` | `store.EvtMCPConfigUnset` |
| `mcp:config:delete` | `store.EvtMCPConfigDelete` |
| `mcp:config:restore` | `store.EvtMCPConfigRestore` |
| `mcp:config:write` | `store.EvtMCPConfigWrite` |
| `mcp:registry:register` | `store.EvtMCPRegistryRegister` |
| `mcp:registry:remove` | `store.EvtMCPRegistryRemove` |
| `mcp:registry:repair` | `store.EvtMCPRegistryRepair` |
| `mcp:registry:unregister` | `store.EvtMCPRegistryUnregister` |
| `mcp:worktree:finalize` | `store.EvtMCPWorktreeFinalize` |
| `mcp:worktree:repair` | `store.EvtMCPWorktreeRepair` |
| `mcp:snapshots:purge` | `store.EvtMCPSnapshotsPurge` |
| `mcp:logs:purge` | `store.EvtMCPLogsPurge` |
| `mcp:es:request` | `store.EvtMCPEsRequest` |
| `mcp:schema:install` | `store.EvtMCPSchemaInstall` |
| `mcp:init:repo` | `store.EvtMCPInitRepo` |
| `mcp:main:worktree` | `store.EvtMCPMainWorktree` |

