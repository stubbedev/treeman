package wt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/patcher"
	"github.com/stubbedev/treeman/internal/ports"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// CreateRequest is the input to Create. RepoRoot must be absolute
// (the caller already resolved --repo / cwd discovery).
type CreateRequest struct {
	RepoRoot    string
	Branch      string
	From        string // base branch override; empty = origin/HEAD
	Path        string // explicit worktree path; empty = derive
	NoFetch     bool
	SkipHooks   bool
	SkipPrepare bool
	Env         map[string]string
}

// CreateStatus categorizes the terminal state of a Create call.
type CreateStatus string

const (
	// CreatedQueued — git add + register succeeded and the daemon
	// accepted the finalize dispatch. Status lines for the heavy
	// tail will appear in the daemon log.
	CreatedQueued CreateStatus = "queued"
	// CreatedNoop — destination already existed and matches an
	// active registry row on the requested branch. No work done.
	CreatedNoop CreateStatus = "noop"
	// CreatedNoFinalize — SkipHooks set, or no hooks/databases
	// configured. Worktree exists, no tail dispatched.
	CreatedNoFinalize CreateStatus = "no_finalize"
)

// CreateResult is the structured outcome of a Create call. Status
// is always set; LogPath is retained for the MCP surface but is unused
// now that the daemon is the sole finalize worker.
//
// JSON tags are snake_case to match the rest of the MCP tool surface
// (callers parse this directly off MCP's structuredContent).
type CreateResult struct {
	WtPath     string            `json:"wt_path"`
	Slug       string            `json:"slug"`
	RepoID     int64             `json:"repo_id"`
	WorktreeID int64             `json:"worktree_id"`
	Status     CreateStatus      `json:"status"`
	LogPath    string            `json:"log_path,omitempty"`
	Ports      map[string]uint16 `json:"ports,omitempty"`
}

// Create runs the full worktree-create lifecycle: git worktree add,
// patch application, sqlite registration, and dispatch of the
// hooks+prepare tail to the daemon (with detached-child fallback).
//
// Output (status lines) is routed through sink so the CLI can color
// + redirect them while MCP receives only the structured result.
//
// The caller is responsible for resolving req.RepoRoot to an
// absolute path before calling.
func Create(ctx context.Context, req CreateRequest, sink Sink) (CreateResult, error) {
	if sink == nil {
		sink = NoopSink{}
	}
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return CreateResult{}, err
	}
	defer func() { _ = st.Close() }()

	res, needsFinalize, err := CreateInStore(ctx, req, st, sink)
	if err != nil || !needsFinalize {
		return res, err
	}
	return finishCreate(ctx, req, res, sink)
}

// CreateInStore is the store-injected core of worktree creation: git
// worktree add + SQLite registration + port allocation, against the
// supplied store (which it does NOT close — the caller owns it). It does
// NOT dispatch the hooks+prepare tail; instead it returns needsFinalize
// to tell the caller whether to run FinalizeWorktree. This is the entry
// point the daemon's worktree_create task uses with its warm store, so
// the registration write lands on the single daemon writer.
//
// Terminal-status cases (noop, no_finalize) set res.Status and return
// needsFinalize=false; the "caller should finalize" case leaves
// res.Status empty and returns needsFinalize=true.
func CreateInStore(ctx context.Context, req CreateRequest, st *store.Store, sink Sink) (CreateResult, bool, error) {
	if sink == nil {
		sink = NoopSink{}
	}
	if req.Branch == "" {
		return CreateResult{}, false, errors.New("branch is required")
	}
	if req.RepoRoot == "" {
		return CreateResult{}, false, errors.New("repo_root is required")
	}

	cfg, err := resolve.LoadResolved(req.RepoRoot)
	if err != nil {
		return CreateResult{}, false, err
	}

	wtPath := req.Path
	if wtPath == "" {
		wtPath = filepath.Join(WorktreesRoot(cfg, req.RepoRoot), req.Branch)
	} else if !filepath.IsAbs(wtPath) {
		wtPath = filepath.Join(req.RepoRoot, wtPath)
	}

	// Idempotency: if the dest exists and matches what we'd create
	// (linked worktree on the requested branch, registered in
	// SQLite), treat the call as a no-op so scripts can retry safely.
	if _, err := os.Stat(wtPath); err == nil {
		if IsMatchingExistingWorktree(ctx, req.RepoRoot, wtPath, req.Branch) {
			sink.Info("worktree already exists at %s on %s — no-op", wtPath, req.Branch)
			return CreateResult{WtPath: wtPath, Status: CreatedNoop}, false, nil
		}
		return CreateResult{}, false, fmt.Errorf("destination path already exists: %s", wtPath)
	}
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return CreateResult{}, false, err
	}

	if err := addGitWorktree(ctx, req, &wtPath); err != nil {
		return CreateResult{}, false, err
	}

	sl := slug.For(wtPath, req.Branch)

	// Register the worktree row + allocate ports up front. File bring-in
	// (worktrees.links / .copies) and patch render run in the daemon
	// finalize tail so the CLI never blocks on a recursive copy; the row
	// + ports must exist first because the daemon keys its work off them.
	reg, err := registerAndAllocate(ctx, req, &cfg, wtPath, sl, st)
	if err != nil {
		return CreateResult{}, false, err
	}
	tplCtx := template.FromSlug(sl).WithPorts(reg.portMap)

	sink.OK("created worktree #%d slug=%s path=%s", reg.wtID, sl.Value, wtPath)
	if summary := ports.FormatSummary(reg.allocs); summary != "" {
		sink.Info("%s", summary)
	}

	result := CreateResult{
		WtPath:     wtPath,
		Slug:       sl.Value,
		RepoID:     reg.repoID,
		WorktreeID: reg.wtID,
		Ports:      reg.portMap,
	}

	return resolveCreateTail(ctx, &cfg, req, wtPath, tplCtx, result, sink)
}

// resolveCreateTail decides the terminal create outcome after the row +
// ports exist: SkipHooks runs the bring-in + patch inline (no daemon
// tail) and reports no_finalize; an otherwise-empty config reports
// no_finalize too; anything that needs hooks/copies/patches/prepare
// reports needsFinalize=true so the caller runs FinalizeWorktree.
func resolveCreateTail(
	ctx context.Context,
	cfg *config.Config,
	req CreateRequest,
	wtPath string,
	tplCtx template.Context,
	result CreateResult,
	sink Sink,
) (CreateResult, bool, error) {
	if req.SkipHooks {
		if err := bringInAndPatch(ctx, cfg, req.RepoRoot, wtPath, tplCtx, sink); err != nil {
			return result, false, err
		}
		result.Status = CreatedNoFinalize
		return result, false, nil
	}
	if !createNeedsWork(cfg, req) {
		result.Status = CreatedNoFinalize
		return result, false, nil
	}
	return result, true, nil
}

// createNeedsWork reports whether a finalize tail has anything to do.
func createNeedsWork(cfg *config.Config, req CreateRequest) bool {
	return len(cfg.Hooks.OnCreateBeforeEngines) > 0 ||
		len(cfg.Hooks.OnCreateAfterEngines) > 0 ||
		len(cfg.Worktrees.Links) > 0 ||
		len(cfg.Worktrees.Copies) > 0 ||
		len(cfg.Patches) > 0 ||
		(!req.SkipPrepare && len(cfg.Databases) > 0)
}

// bringInAndPatch materializes the per-worktree files `wt create` used to
// handle inline: symlink worktrees.links, copy worktrees.copies, then
// render patches against the allocated ports (tplCtx). Normally this work
// is offloaded to the daemon (FinalizeWorktree) so the CLI never blocks
// on a recursive copy; this inline variant backs the SkipHooks path,
// which dispatches no daemon tail.
func bringInAndPatch(
	ctx context.Context,
	cfg *config.Config,
	repoRoot, wtPath string,
	tplCtx template.Context,
	sink Sink,
) error {
	if err := BringInFiles(ctx, repoRoot, wtPath, cfg.Worktrees.Links, "link", sink); err != nil {
		return err
	}
	if err := BringInFiles(ctx, repoRoot, wtPath, cfg.Worktrees.Copies, "copy", sink); err != nil {
		return err
	}
	return applyPatches(ctx, cfg.Patches, wtPath, tplCtx, sink)
}

// addGitWorktree decides the base ref (with an optional pre-fetch),
// runs `git worktree add`, and resolves the worktree path to absolute.
// Mutates *wtPath in place. Extracted from Create as a mechanical lift
// of the git-creation phase.
func addGitWorktree(ctx context.Context, req CreateRequest, wtPath *string) error {
	// Clear stale worktree admin entries whose working dir was removed
	// out-of-band — e.g. a prior create whose finalize failed and got
	// reaped, leaving `.git/worktrees/<name>` behind. Without this,
	// `git worktree add` aborts with "fatal: '<branch>' is already
	// checked out at '<gone path>'" (exit 128) even though the dir no
	// longer exists. Best-effort: a prune failure must not block create.
	_ = gitcmd.RunPiped(ctx, req.RepoRoot, nil, nil, "worktree", "prune")

	// Decide base ref + optional pre-fetch.
	base := req.From
	if base == "" {
		base = DetectDefaultBranch(ctx, req.RepoRoot)
	}
	branchExists := gitcmd.Exists(ctx, req.RepoRoot, "refs/heads/"+req.Branch)
	if !branchExists && !req.NoFetch {
		_ = gitcmd.RunPiped(ctx, req.RepoRoot, nil, nil, "fetch", "origin", base, "--quiet")
		if RefExistsRemote(ctx, req.RepoRoot, base) {
			base = "origin/" + base
		}
	}
	var gitArgs []string
	if branchExists {
		gitArgs = []string{"worktree", "add", *wtPath, req.Branch}
	} else {
		gitArgs = []string{"worktree", "add", "-b", req.Branch, *wtPath, base}
	}
	// Route git's output to stderr (not stdout) so the --print-path
	// shell idiom — `cd "$(treeman worktree create x --print-path)"` —
	// doesn't ingest "Preparing worktree …" / "HEAD is now at …"
	// lines that git emits on its stdout. Tee stderr through a buffer
	// too: RunPiped streams stderr to the writer and drops it from the
	// returned error, so without this MCP / non-TTY callers see only a
	// bare "exit status 128" instead of git's actual complaint (e.g.
	// "'<branch>' is already checked out at '<path>'").
	var errBuf bytes.Buffer
	if err := gitcmd.RunPiped(ctx, req.RepoRoot, os.Stderr, io.MultiWriter(os.Stderr, &errBuf), gitArgs...); err != nil {
		if tail := strings.TrimSpace(errBuf.String()); tail != "" {
			return fmt.Errorf("git worktree add: %w: %s", err, tail)
		}
		return fmt.Errorf("git worktree add: %w", err)
	}
	if abs, err := filepath.Abs(*wtPath); err == nil {
		*wtPath = abs
	}
	return nil
}

// registration bundles the derived ids/ports a Create call needs after
// the worktree row is registered.
type registration struct {
	repoID  int64
	wtID    int64
	allocs  []ports.Allocation
	portMap map[string]uint16
}

// registerAndAllocate registers the repo + worktree rows and allocates
// the configured ports against the supplied store (which it does NOT
// own — no open/close here).
func registerAndAllocate(
	ctx context.Context,
	req CreateRequest,
	cfg *config.Config,
	wtPath string,
	sl slug.Slug,
	st *store.Store,
) (registration, error) {
	repoID, err := st.EnsureRepo(ctx, req.RepoRoot, filepath.Base(req.RepoRoot))
	if err != nil {
		return registration{}, err
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, req.Branch)
	if err != nil {
		return registration{}, err
	}
	allocs, err := ports.New().Allocate(ctx, st, cfg, repoID, wtID)
	if err != nil {
		return registration{}, fmt.Errorf("port allocation: %w", err)
	}
	portMap := map[string]uint16{}
	for _, a := range allocs {
		portMap[a.Name] = a.Port
	}
	return registration{repoID: repoID, wtID: wtID, allocs: allocs, portMap: portMap}, nil
}

// finishCreate dispatches the hooks+prepare tail for the in-process
// Create path (used by tests + any non-daemon caller): daemon RPC first,
// then a detached setsid child when the daemon is unreachable. The daemon
// itself never reaches here — its worktree_create task runs
// FinalizeWorktree directly instead.
func finishCreate(ctx context.Context, req CreateRequest, result CreateResult, sink Sink) (CreateResult, error) {
	if queued := DispatchFinalize(ctx, req.RepoRoot, result.WtPath, req.Env, sink); queued {
		result.Status = CreatedQueued
		return result, nil
	}
	// The worktree + registry row exist, but the daemon — the async
	// finalize worker — could not be reached even after an autostart
	// attempt (see CallWithStart/EnsureDaemon). We do not fork a CLI
	// child to run the tail; report it so the user can start the daemon
	// and re-run finalize.
	return result, fmt.Errorf(
		"daemon unreachable — worktree %q created but setup + prepare did not run; "+
			"start the daemon and run `treeman worktree finalize %s`; %s",
		result.WtPath, result.WtPath, daemonDebugHint())
}

// applyPatches runs every configured top-level patch against the new
// worktree, then wires git's clean/smudge filter for the same files so
// `git pull` can overwrite them without tripping the merge guard.
// Extracted from Create as a mechanical lift of the patch phase.
func applyPatches(ctx context.Context, patches []config.Patch, wtPath string, tplCtx template.Context, sink Sink) error {
	for _, p := range patches {
		res, err := patcher.Apply(p, wtPath, tplCtx)
		if err != nil {
			return err
		}
		if res.Outcome == patcher.Updated {
			sink.Info("patched %s (%s)", filepath.Join(wtPath, res.File), res.Driver)
		}
	}
	if len(patches) > 0 {
		files := make([]string, 0, len(patches))
		for _, p := range patches {
			files = append(files, p.File)
		}
		if err := patcher.EnsureFilter(ctx, wtPath, files); err != nil {
			sink.Warn("install patch filter: %v", err)
		}
	}
	return nil
}
