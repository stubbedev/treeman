package store

// Event types — the canonical `event_type` values written to the
// SQLite event log via WriteEvent. SINGLE SOURCE OF TRUTH: every
// emitter references these constants, never a string literal, so a
// rename is one edit here. logs_query / CLI filters and the
// notification classifier (internal/notify) match against these too.
//
// Scheme: `domain:object:stage`, colon = hierarchy. domain is the
// config option the event relates to (copies, ports, snapshots,
// auto_fetch→fetch, …) or a subsystem (worktree, daemon, mcp).
// Stage vocabulary is closed: start / end / error / skip / cancel for
// bracketed operations; a bare present-tense verb for one-shots.
const (
	// worktrees.copies / worktrees.links — bring-in passes.
	EvtCopiesStart = "copies:start"
	EvtCopiesPath  = "copies:path"
	EvtCopiesEnd   = "copies:end"
	EvtLinksStart  = "links:start"
	EvtLinksPath   = "links:path"
	EvtLinksEnd    = "links:end"

	// ports
	EvtPortsAllocate = "ports:allocate"
	EvtPortsError    = "ports:error"

	// patches
	EvtPatchesApply = "patches:apply"

	// hooks
	EvtHooksStart = "hooks:start"
	EvtHooksEnd   = "hooks:end"

	// databases — prepare pipeline
	EvtPrepareStart               = "prepare:start"
	EvtPreparePhase               = "prepare:phase"
	EvtPrepareEnd                 = "prepare:end"
	EvtPrepareError               = "prepare:error"
	EvtPrepareSkip                = "prepare:skip"
	EvtPrepareIncrementalStart    = "prepare:incremental:start"
	EvtPrepareIncrementalFallback = "prepare:incremental:fallback"
	EvtPrepareDumpOnlyStart       = "prepare:dumponly:start"
	EvtPrepareDumpOnlyFallback    = "prepare:dumponly:fallback"
	EvtPrepareRollbackStart       = "prepare:rollback:start"
	EvtPrepareRollbackFallback    = "prepare:rollback:fallback"
	EvtPrepareUnsupported         = "prepare:unsupported"

	// databases — db operations
	EvtDBDrop          = "db:drop"
	EvtDBReset         = "db:reset"
	EvtDBSave          = "db:save"
	EvtDBTeardownSkip  = "db:teardown:skip"
	EvtDBTeardownError = "db:teardown:error"

	// databases[].migrate
	EvtMigrateSkip = "migrate:skip"

	// databases[].test_clones — fanout + per-clone restore
	EvtClonesStart        = "clones:start"
	EvtClonesEnd          = "clones:end"
	EvtClonesRestoreEnd   = "clones:restore:end"
	EvtClonesRestoreError = "clones:restore:error"

	// snapshots
	EvtSnapshotsCacheHit      = "snapshots:cache:hit"
	EvtSnapshotsCacheMiss     = "snapshots:cache:miss"
	EvtSnapshotsCacheFallback = "snapshots:cache:fallback"
	EvtSnapshotsStrategy      = "snapshots:strategy"
	EvtSnapshotsEvictCap      = "snapshots:evict:cap"
	EvtSnapshotsEvictSource   = "snapshots:evict:source"
	EvtSnapshotsEvictAge      = "snapshots:evict:age"
	EvtSnapshotsEvictSize     = "snapshots:evict:size"
	EvtSnapshotsPurge         = "snapshots:purge"
	EvtSnapshotsPurgeEnd      = "snapshots:purge:end"
	EvtSnapshotsPrewarm       = "snapshots:prewarm"
	EvtSnapshotsPrewarmClaim  = "snapshots:prewarm:claim"

	// auto_fetch — fetch + branch maintenance
	EvtFetchPull          = "fetch:pull"
	EvtFetchSkip          = "fetch:skip"
	EvtFetchError         = "fetch:error"
	EvtFetchRebaseError   = "fetch:rebase:error"
	EvtBranchPrune        = "branch:prune"
	EvtBranchReap         = "branch:reap"
	EvtBranchCaptureSkip  = "branch:capture:skip"
	EvtBranchCaptureError = "branch:capture:error"

	// databases[].inputs + HEAD — watcher
	EvtWatchStart = "watch:start"
	EvtWatchInput = "watch:input"
	EvtWatchHead  = "watch:head"

	// main_worktree
	EvtMainEnroll  = "main:enroll"
	EvtMainDisable = "main:disable"
	EvtMainPurge   = "main:purge"

	// worktree lifecycle (no config key)
	EvtWorktreeCreateStart    = "worktree:create:start"
	EvtWorktreeCreateCheckout = "worktree:create:checkout"
	EvtWorktreeCreateEnd      = "worktree:create:end"
	EvtWorktreeCreateError    = "worktree:create:error"
	EvtWorktreeCreateCancel   = "worktree:create:cancel"
	EvtWorktreeCreateDeferred = "worktree:create:deferred"
	EvtWorktreeDeleteStart    = "worktree:delete:start"
	EvtWorktreeDeleteEnd      = "worktree:delete:end"
	EvtWorktreeDeleteError    = "worktree:delete:error"
	EvtWorktreeReapStart      = "worktree:reap:start"
	EvtWorktreeReapEnd        = "worktree:reap:end"
	EvtWorktreeRecoverDrop    = "worktree:recover:drop"
	EvtWorktreeRecoverError   = "worktree:recover:error"

	// daemon / global
	EvtDaemonStart    = "daemon:start"
	EvtDaemonStop     = "daemon:stop"
	EvtConfigReload   = "config:reload"
	EvtRegistryRemove = "registry:remove"
	EvtPlanStart      = "plan:start"
	EvtPlanEnd        = "plan:end"
	EvtPlanError      = "plan:error"

	// MCP audit events — one per mutating MCP tool. Explicit constants
	// (not derived from the tool name) so the event type is greppable
	// and survives a tool rename independently.
	EvtMCPConfigSet          = "mcp:config:set"
	EvtMCPConfigUnset        = "mcp:config:unset"
	EvtMCPConfigDelete       = "mcp:config:delete"
	EvtMCPConfigRestore      = "mcp:config:restore"
	EvtMCPConfigWrite        = "mcp:config:write"
	EvtMCPRegistryRegister   = "mcp:registry:register"
	EvtMCPRegistryRemove     = "mcp:registry:remove"
	EvtMCPRegistryRepair     = "mcp:registry:repair"
	EvtMCPRegistryUnregister = "mcp:registry:unregister"
	EvtMCPWorktreeFinalize   = "mcp:worktree:finalize"
	EvtMCPWorktreeRepair     = "mcp:worktree:repair"
	EvtMCPSnapshotsPurge     = "mcp:snapshots:purge"
	EvtMCPLogsPurge          = "mcp:logs:purge"
	EvtMCPEsRequest          = "mcp:es:request"
	EvtMCPSchemaInstall      = "mcp:schema:install"
	EvtMCPInitRepo           = "mcp:init:repo"
	EvtMCPMainWorktree       = "mcp:main:worktree"
)
