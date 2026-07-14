package wt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// run the teardown inline in this process. A manual escape hatch
	// (debugging / daemon-less teardown); nothing sets it automatically.
	Detached bool
}

// DeleteStatus categorizes the terminal state of a Delete call.
type DeleteStatus string

const (
	// DeleteQueued — daemon accepted the teardown dispatch.
	DeleteQueued DeleteStatus = "queued"
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
// teardown to the daemon over the socket (falling back to an
// in-process teardown when it's unreachable) or runs it inline when
// req.Detached is set.
//
// Confirmation is the caller's responsibility — Delete itself is
// non-interactive.
func Delete(ctx context.Context, req DeleteRequest, sink Sink) (DeleteResult, error) {
	if sink == nil {
		sink = NoopSink{}
	}
	if req.Target == "" {
		return DeleteResult{}, errors.New("target is required")
	}
	if req.RepoRoot == "" {
		return DeleteResult{}, errors.New("repo_root is required")
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
			return DeleteResult{}, fmt.Errorf(
				"no worktree matches %q in %s (use --force to remove a stale registry entry)",
				req.Target,
				req.RepoRoot,
			)
		}
	}

	// Refuse to delete the repo's primary checkout. The main worktree IS
	// the repo root; `git worktree remove` can't remove it anyway, and the
	// teardown that runs first would drop its databases — for a
	// branch_scoped config that's the bare, overlay-resolved app DB the
	// repo-root `.env` points at. Linked worktrees live under the
	// worktrees dir, never at the repo root, so this only trips on a
	// main-worktree target.
	if pathsEqual(wtPath, req.RepoRoot) {
		return DeleteResult{}, fmt.Errorf(
			"refusing to delete %q: it is the repo's main worktree (primary checkout), not a linked worktree",
			wtPath,
		)
	}

	// Double-delete guard: a teardown already in flight for this path
	// means the daemon is mid DB-drop/hooks — dispatching a second one
	// would race it. LookupWorktree above already skips tearing-down
	// rows, but an explicit path target bypasses the lookup, so check
	// the path directly. Force stays as the escape hatch for a
	// teardown that hung without landing delete:end / delete:error.
	if !req.Force && teardownInFlight(ctx, req.RepoRoot, wtPath) {
		return DeleteResult{}, fmt.Errorf(
			"teardown already in progress for %s (use --force to run it again)", wtPath)
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
	// Daemon unreachable even after an autostart attempt (see
	// CallWithStart/EnsureDaemon). Run the teardown in THIS process —
	// the same daemon-less fallback every other command uses via
	// submitPlan. Blocks until done (rm -rf + DB drops run foreground),
	// but never forks a CLI child and never strands the worktree.
	sink.Warn("daemon unreachable — running teardown in-process (blocks until done; `treeman daemon start` restores detached dispatch)")
	if err := inlineTeardown(ctx, req.RepoRoot, wtPath, req.Force, req.Env, sink); err != nil {
		return DeleteResult{WtPath: wtPath}, err
	}
	return DeleteResult{WtPath: wtPath, Status: DeleteInline}, nil
}

// teardownInFlight reports whether the live registry row at wtPath is
// mid-teardown (latest delete event is delete:start). Best-effort: an
// unreadable store answers false and the delete proceeds.
func teardownInFlight(ctx context.Context, repoRoot, wtPath string) bool {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return false
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return false
	}
	defer func() { _ = st.Close() }()
	var one int
	qErr := st.DB.QueryRowContext(ctx, `
		SELECT 1 FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? COLLATE NOCASE AND w.path = ? COLLATE NOCASE
		AND w.deleted_at IS NULL AND NOT (`+store.WorktreeNotTearingDown+`)`,
		repoRoot, wtPath).Scan(&one)
	return qErr == nil
}

// pathsEqual reports whether two paths point at the same directory.
// Case-insensitive to match the store's NOCASE path keys (an
// APFS/Windows case-insensitive FS treats them as one location).
func pathsEqual(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// inlineTeardown runs the full teardown sequence locally without
// dispatching to the daemon. Used by the explicit `--detached` mode
// and as the automatic fallback when the daemon is unreachable.
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
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	branch := gitenv.DetectBranch(ctx, wtPath)
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
	runTrigger("delete-before-engines", cfg.Hooks.OnDeleteBeforeEngines)
	if err := prepare.TeardownDatabases(ctx, &cfg, id.Slug.Value, repoID, id.WtID, st); err != nil {
		slog.Warn("wt delete: teardown databases",
			"slug", id.Slug.Value, "repo", repoRoot, "wt", wtPath, "err", err)
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtWorktreeDeleteError,
			fmt.Sprintf("teardown databases: %v", err),
			repoID, id.WtID, "", 0, map[string]any{
				"slug": id.Slug.Value, "err": err.Error(),
			})
	}
	runTrigger("delete-after-engines", cfg.Hooks.OnDeleteAfterEngines)
	// Release the per-worktree port reservations back into the pool
	// so a future `wt create` can re-use them.
	if err := st.ReleaseWorktreePorts(ctx, id.WtID); err != nil {
		slog.Warn("wt delete: release ports (freed ports stay blocked)", "wt", wtPath, "err", err)
	}
	// Drop every active-branch marker for this worktree. teardownBranchScoped
	// only clears markers for currently-configured branch_scoped databases;
	// this bulk clear also reaps markers for databases since removed from
	// config, so a re-created worktree at the same path starts clean.
	if err := st.ClearActiveBranchesForWorktree(ctx, id.WtID); err != nil {
		slog.Warn("wt delete: clear active-branch markers (stale markers survive recreate)", "wt", wtPath, "err", err)
	}
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
	// git worktree remove leaves untracked copies/links bring-in behind
	// (node_modules, vendored dirs, nested git repos). Nuke it plus the
	// now-empty parent dirs so a future create at this path doesn't trip
	// "destination path already exists".
	RemoveWorktreeTree(wtPath, WorktreesRoot(cfg, repoRoot))
	_ = sink
	return nil
}
