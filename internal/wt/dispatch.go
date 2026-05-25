package wt

import (
	"context"

	"github.com/stubbedev/treeman/internal/rpc"
)

// DispatchFinalize hands setup + prepare to the daemon. Returns true
// when the daemon successfully queued the work. See dispatchToDaemon
// for the retry semantics.
func DispatchFinalize(ctx context.Context, repoRoot, wtPath string, env map[string]string, sink Sink) bool {
	req := rpc.Request{
		Method: rpc.MethodWorktreeFinalize,
		WorktreeFinalize: &rpc.WorktreeFinalizeArgs{
			RepoPath:     repoRoot,
			WorktreePath: wtPath,
			InheritedEnv: env,
		},
	}
	return dispatchToDaemon(ctx, req, rpc.KindWorktreeFinalizeQueued,
		"setup + prepare detached to daemon — follow with `treeman logs tail --follow`",
		"setup + prepare detached to daemon (auto-restarted)",
		sink)
}

// DispatchTeardown is the wt-delete twin of DispatchFinalize.
func DispatchTeardown(ctx context.Context, repoRoot, wtPath string, force bool, env map[string]string, sink Sink) bool {
	req := rpc.Request{
		Method: rpc.MethodWorktreeTeardown,
		WorktreeTeardown: &rpc.WorktreeTeardownArgs{
			RepoPath:     repoRoot,
			WorktreePath: wtPath,
			Force:        force,
			InheritedEnv: env,
		},
	}
	return dispatchToDaemon(ctx, req, rpc.KindWorktreeTeardownQueued,
		"teardown + DB teardown + git remove detached to daemon — follow with `treeman logs tail --follow`",
		"teardown + DB teardown + git remove detached to daemon (auto-restarted)",
		sink)
}

// dispatchToDaemon issues an RPC, emits okMsg via sink + returns
// true if the daemon responded with wantKind. On RPC failure, runs
// EnsureDaemon and retries once; on retry success, emits restartedMsg
// instead. Returns false when neither attempt succeeds — callers
// then fall back to a detached child / inline run.
func dispatchToDaemon(ctx context.Context, req rpc.Request, wantKind, okMsg, restartedMsg string, sink Sink) bool {
	if sink == nil {
		sink = NoopSink{}
	}
	resp, err := rpc.Call(ctx, req)
	if err == nil {
		if resp.Kind == wantKind {
			sink.OK("queued: %s", okMsg)
			return true
		}
		if resp.Kind == rpc.KindError {
			sink.Warn("daemon: %s", resp.Message)
		}
		return false
	}
	sink.Warn("daemon RPC failed (%v); trying daemon restart", err)
	if startErr := EnsureDaemon(ctx); startErr != nil {
		return false
	}
	if resp, err := rpc.Call(ctx, req); err == nil && resp.Kind == wantKind {
		sink.OK("queued: %s", restartedMsg)
		return true
	}
	return false
}
