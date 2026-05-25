// Package wt is the worktree-lifecycle orchestrator. The CLI's
// `wt create` / `wt delete` commands and the MCP server's
// worktree_create / worktree_delete tools both delegate to this
// package so they share a single source of truth for:
//
//   - git worktree add + patch application + sqlite registration,
//   - dispatching the heavy tail (hooks + db prepare) to the
//     daemon, with a setsid-child detached fallback when the
//     daemon is unreachable,
//   - resolving + teardown of an existing worktree.
//
// Callers pass a Sink so the CLI can render status lines through
// its ui package while MCP receives only the structured result.
package wt

// Sink is the user-facing progress channel for an orchestrator run.
// The CLI wires this to its colored Print* helpers; MCP wires it to
// NoopSink and reads the structured Result instead.
type Sink interface {
	OK(format string, args ...any)
	Warn(format string, args ...any)
	Info(format string, args ...any)
}

// NoopSink discards every message. Use from non-interactive callers
// (MCP, tests) that read the structured Result and don't need
// running commentary.
type NoopSink struct{}

func (NoopSink) OK(string, ...any)   {}
func (NoopSink) Warn(string, ...any) {}
func (NoopSink) Info(string, ...any) {}
