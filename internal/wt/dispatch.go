package wt

import (
	"context"

	"github.com/stubbedev/treeman/internal/rpc"
)

// DispatchFinalize hands setup + prepare to the daemon. Returns true
// when the daemon successfully queued the work.
func DispatchFinalize(ctx context.Context, repoRoot, wtPath string, env map[string]string, sink Sink) bool {
	req := rpc.Plan(false, rpc.One(rpc.Task{
		Type:         rpc.TaskWorktreeFinalize,
		RepoPath:     repoRoot,
		WorktreePath: wtPath,
		InheritedEnv: env,
	}))
	return dispatchToDaemon(ctx, req,
		"setup + prepare detached to daemon — follow with `treeman logs tail --follow`", sink)
}

// DispatchTeardown is the wt-delete twin of DispatchFinalize.
func DispatchTeardown(ctx context.Context, repoRoot, wtPath string, force bool, env map[string]string, sink Sink) bool {
	task := rpc.Task{
		Type:         rpc.TaskWorktreeTeardown,
		RepoPath:     repoRoot,
		WorktreePath: wtPath,
		InheritedEnv: env,
	}
	if force {
		task.Params = map[string]string{"force": "1"}
	}
	return dispatchToDaemon(ctx, rpc.Plan(false, rpc.One(task)),
		"teardown + DB teardown + git remove detached to daemon — follow with `treeman logs tail --follow`", sink)
}

// CallWithStart issues an RPC and, on a connection failure, starts the
// daemon once and retries. This is the single home of the "autostart
// treemand" policy — CLI and MCP both route their plan submissions
// through it instead of hand-rolling the retry.
func CallWithStart(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	resp, err := rpc.Call(ctx, req)
	if err == nil {
		return resp, nil
	}
	if startErr := EnsureDaemon(ctx); startErr != nil {
		return rpc.Response{}, err
	}
	return rpc.Call(ctx, req)
}

// dispatchToDaemon submits a queued plan, emits okMsg via sink, and
// returns true when the daemon accepted it (KindPlanQueued). Returns
// false on error so the caller can decide how to handle an unreachable
// daemon (create/delete surface a hard error).
func dispatchToDaemon(ctx context.Context, req rpc.Request, okMsg string, sink Sink) bool {
	if sink == nil {
		sink = NoopSink{}
	}
	resp, err := CallWithStart(ctx, req)
	if err != nil {
		sink.Warn("daemon RPC failed: %v", err)
		return false
	}
	if resp.Kind == rpc.KindPlanQueued {
		sink.OK("queued: %s", okMsg)
		return true
	}
	if resp.Kind == rpc.KindError {
		sink.Warn("daemon: %s", resp.Message)
	}
	return false
}
