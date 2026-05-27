package wt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
)

// DeleteRequest is the input to Delete. Target may be a path,
// branch, slug, or basename — Delete resolves it against the
// registry first, falling back to interpreting it as a path
// when no match is found (gated by Force when the path is gone).
type DeleteRequest struct {
	RepoRoot string
	Target   string
	Force    bool
	Env      map[string]string

	// Detached is the internal mode used by the CLI's
	// `wt delete --detached` subcommand: skip daemon dispatch and
	// run the teardown inline in this process. Set by the
	// detached-child path; MCP and the user-facing CLI never set it.
	Detached bool
}

// DeleteStatus categorizes the terminal state of a Delete call.
type DeleteStatus string

const (
	// DeleteQueued — daemon accepted the teardown dispatch.
	DeleteQueued DeleteStatus = "queued"
	// DeleteDetached — daemon was unreachable; teardown is running
	// in a setsid child whose log path is LogPath.
	DeleteDetached DeleteStatus = "detached"
	// DeleteInline — caller asked for inline mode (Detached=true);
	// the teardown ran fully in this process and is now complete.
	DeleteInline DeleteStatus = "inline"
)

// DeleteResult is the structured outcome of a Delete call.
//
// JSON tags are snake_case to match the rest of the MCP tool surface.
type DeleteResult struct {
	WtPath  string       `json:"wt_path"`
	Status  DeleteStatus `json:"status"`
	LogPath string       `json:"log_path,omitempty"`
}

// Delete resolves the target worktree and either dispatches the
// teardown to the daemon (with detached-child fallback) or runs it
// inline when req.Detached is set.
//
// Confirmation is the caller's responsibility — Delete itself is
// non-interactive.
func Delete(ctx context.Context, req DeleteRequest, sink Sink) (DeleteResult, error) {
	if sink == nil {
		sink = NoopSink{}
	}
	if req.Target == "" {
		return DeleteResult{}, fmt.Errorf("target is required")
	}
	if req.RepoRoot == "" {
		return DeleteResult{}, fmt.Errorf("repo_root is required")
	}

	// Registry lookup wins over path interpretation. With Force we
	// fall back to path-as-typed even when the directory is gone.
	var wtPath string
	if p, ok := LookupWorktree(ctx, req.RepoRoot, req.Target, sink); ok {
		wtPath = p
	} else {
		abs, err := filepath.Abs(req.Target)
		if err != nil {
			return DeleteResult{}, err
		}
		wtPath = abs
		if _, statErr := os.Stat(wtPath); statErr != nil && !req.Force {
			return DeleteResult{}, fmt.Errorf("no worktree matches %q in %s (use --force to remove a stale registry entry)", req.Target, req.RepoRoot)
		}
	}

	if req.Detached {
		if err := inlineTeardown(ctx, req.RepoRoot, wtPath, req.Force, req.Env, sink); err != nil {
			return DeleteResult{WtPath: wtPath}, err
		}
		return DeleteResult{WtPath: wtPath, Status: DeleteInline}, nil
	}

	if queued := DispatchTeardown(ctx, req.RepoRoot, wtPath, req.Force, req.Env, sink); queued {
		return DeleteResult{WtPath: wtPath, Status: DeleteQueued}, nil
	}
	logPath, err := DetachDelete(wtPath, req.RepoRoot, req.Force)
	if err != nil {
		return DeleteResult{WtPath: wtPath}, fmt.Errorf("detach teardown: %w", err)
	}
	sink.OK("queued: teardown + DB drop + git remove detached (daemon unreachable — log: %s)", logPath)
	return DeleteResult{WtPath: wtPath, Status: DeleteDetached, LogPath: logPath}, nil
}

// inlineTeardown runs the full teardown sequence locally without
// dispatching to the daemon. Only used by the `--detached` child
// path; the user-facing wt-delete always returns immediately after
// dispatch.
func inlineTeardown(ctx context.Context, repoRoot, wtPath string, force bool, env map[string]string, sink Sink) error {
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return err
	}
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	branch := gitenv.DetectBranch(wtPath)
	id, err := ResolveIdentity(ctx, st, &cfg, repoRoot, wtPath, branch, repoID)
	if err != nil {
		return err
	}
	runTrigger := func(trigger string, actions []config.Action) {
		if len(actions) == 0 {
			return
		}
		started := hooks.EmitHookStart(ctx, st, repoID, id.WtID, trigger, len(actions))
		out, _ := hooks.RunHooks(ctx, trigger, actions, repoRoot, wtPath, id.Slug.Value, id.IsMain, env, true)
		hooks.PersistOutcome(ctx, st, repoID, id.WtID, trigger, started, time.Now().UnixMilli(), out)
	}
	runTrigger("on-delete-before-engines", cfg.Hooks.OnDeleteBeforeEngines)
	_ = prepare.TeardownDatabases(ctx, &cfg, id.Slug.Value, repoID, id.WtID, st)
	runTrigger("on-delete-after-engines", cfg.Hooks.OnDeleteAfterEngines)
	// Release the per-worktree port reservations back into the pool
	// so a future `wt create` can re-use them.
	_ = st.ReleaseWorktreePorts(ctx, id.WtID)
	_ = st.MarkWorktreeDeleted(ctx, id.WtID)
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wtPath)
	// Same stdout-discipline rationale as Create's git invocation:
	// route everything informational to stderr.
	if err := gitcmd.RunPiped(ctx, repoRoot, os.Stderr, os.Stderr, args...); err != nil && !force {
		return fmt.Errorf("git worktree remove: %w", err)
	}
	PruneEmptyParents(wtPath, WorktreesRoot(cfg, repoRoot))
	_ = sink
	return nil
}
