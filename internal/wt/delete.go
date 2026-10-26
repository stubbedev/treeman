package wt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/stubbedev/treeman/internal/slug"
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
	p, ok, err := LookupWorktree(ctx, req.RepoRoot, req.Target, sink)
	if err != nil {
		return DeleteResult{}, err
	}
	if ok {
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
	if !req.Force {
		inFlight, err := teardownInFlight(ctx, req.RepoRoot, wtPath)
		if err != nil {
			return DeleteResult{}, err
		}
		if inFlight {
			return DeleteResult{}, fmt.Errorf(
				"teardown already in progress for %s (use --force to run it again)", wtPath)
		}
	}

	if err := EnsureNotInUse(ctx, wtPath); err != nil {
		return DeleteResult{WtPath: wtPath}, err
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

func teardownInFlight(ctx context.Context, repoRoot, wtPath string) (bool, error) {
	st, err := openSharedDefaultStore(ctx)
	if err != nil {
		return false, err
	}
	var one int
	err = st.DB.QueryRowContext(ctx, `
		SELECT 1 FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? COLLATE NOCASE AND w.path = ? COLLATE NOCASE
		AND w.deleted_at IS NULL AND NOT (`+store.WorktreeNotTearingDown+`)`,
		repoRoot, wtPath).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check whether a teardown of %s is already running: %w", wtPath, err)
	}
	return true, nil
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
func inlineTeardown(ctx context.Context, repoRoot, wtPath string, force bool, env map[string]string, sink Sink) (err error) {
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return err
	}
	st, err := openSharedDefaultStore(ctx)
	if err != nil {
		return err
	}
	// A worktree whose directory is already gone (removed by hand, then
	// `worktree prune`) has no identity left on disk, and ResolveIdentity
	// refuses a path that is not a linked worktree. Like the daemon's
	// teardownOrphan, take it from the registry row, run the hooks from
	// the repo root with their logs under XDG_STATE_HOME, and skip the
	// git removal: there is no worktree left to remove.
	_, statErr := os.Stat(wtPath)
	gone := errors.Is(statErr, fs.ErrNotExist)
	var (
		repoID int64
		id     Identity
	)
	if gone {
		row, lookupErr := st.LookupWorktreeByPath(ctx, wtPath)
		if lookupErr != nil {
			return fmt.Errorf("look up the registry row of %s: %w", wtPath, lookupErr)
		}
		if row.ID == 0 || row.Deleted {
			return nil
		}
		repoID = row.RepoID
		id = Identity{Slug: slug.Slug{Value: row.Slug}, WtID: row.ID, Branch: row.Branch, IsMain: row.IsMain}
	} else {
		repoID, err = st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
		if err != nil {
			return fmt.Errorf("register repo %s for the teardown of %s: %w", repoRoot, wtPath, err)
		}
		branch := gitenv.DetectBranch(ctx, wtPath)
		id, err = ResolveIdentity(ctx, st, &cfg, repoRoot, wtPath, branch, repoID)
		if err != nil {
			return err
		}
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeDeleteStart,
		"in-process teardown hooks + db teardown + git remove beginning", repoID, id.WtID, "", 0, nil)
	defer func() {
		if err != nil {
			_ = st.WriteEvent(context.WithoutCancel(ctx), store.LevelError, store.EvtWorktreeDeleteError,
				err.Error(), repoID, id.WtID, "", 0, map[string]string{"worktree_path": wtPath})
			return
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeDeleteEnd,
			"in-process teardown complete", repoID, id.WtID, "", 0, nil)
	}()
	runTrigger := func(trigger string, actions []config.Action) {
		if len(actions) == 0 {
			return
		}
		started := hooks.EmitHookStart(ctx, st, repoID, id.WtID, trigger, len(actions))
		var (
			out     hooks.RunOutcome
			hookErr error
		)
		if gone {
			out, hookErr = hooks.RunHooksOrphan(ctx, trigger, actions, repoRoot, wtPath, id.Slug.Value,
				hooks.OrphanLogDir(repoRoot, id.Slug.Value), id.IsMain, env, true)
		} else {
			out, hookErr = hooks.RunHooks(ctx, trigger, actions, repoRoot, wtPath, id.Slug.Value, id.IsMain, env, true)
		}
		hooks.PersistOutcome(ctx, st, repoID, id.WtID, trigger, started, time.Now().UnixMilli(), out)
		if hookErr != nil {
			sink.Warn("%s hooks failed for %s (teardown continues): %v", trigger, wtPath, hookErr)
		}
	}
	runTrigger("delete-before-engines", cfg.Hooks.OnDeleteBeforeEngines)
	if err := prepare.TeardownDatabases(ctx, &cfg, id.Slug.Value, repoID, id.WtID, st); err != nil {
		sink.Warn("dropping the databases of %s failed; the daemon retries it on its next sweep: %v", wtPath, err)
	}
	runTrigger("delete-after-engines", cfg.Hooks.OnDeleteAfterEngines)
	if !gone {
		if err := gitcmd.RunPiped(ctx, repoRoot, os.Stderr, os.Stderr, "worktree", "remove", "--force", wtPath); err != nil && !force {
			return fmt.Errorf("git worktree remove %s: %w", wtPath, err)
		}
		RemoveWorktreeTree(wtPath, WorktreesRoot(cfg, repoRoot))
	}
	if err := st.ReleaseWorktreePorts(ctx, id.WtID); err != nil {
		sink.Warn("releasing the ports of %s failed; they stay reserved: %v", wtPath, err)
	}
	if err := st.MarkWorktreeDeleted(ctx, id.WtID); err != nil {
		return fmt.Errorf("mark worktree %s deleted: %w", wtPath, err)
	}
	return nil
}
