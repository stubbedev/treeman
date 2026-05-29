package wt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	// CreatedDetached — daemon was unreachable; the heavy tail is
	// running in a setsid child whose log path is LogPath.
	CreatedDetached CreateStatus = "detached"
	// CreatedNoop — destination already existed and matches an
	// active registry row on the requested branch. No work done.
	CreatedNoop CreateStatus = "noop"
	// CreatedNoFinalize — SkipHooks set, or no hooks/databases
	// configured. Worktree exists, no tail dispatched.
	CreatedNoFinalize CreateStatus = "no_finalize"
)

// CreateResult is the structured outcome of a Create call. Status
// is always set; LogPath is populated only when Status==CreatedDetached.
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
	if req.Branch == "" {
		return CreateResult{}, errors.New("branch is required")
	}
	if req.RepoRoot == "" {
		return CreateResult{}, errors.New("repo_root is required")
	}

	cfg, err := resolve.LoadResolved(req.RepoRoot)
	if err != nil {
		return CreateResult{}, err
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
			return CreateResult{WtPath: wtPath, Status: CreatedNoop}, nil
		}
		return CreateResult{}, fmt.Errorf("destination path already exists: %s", wtPath)
	}
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return CreateResult{}, err
	}

	if err := addGitWorktree(ctx, req, &wtPath); err != nil {
		return CreateResult{}, err
	}

	// worktrees.links — read-only symlinks from main into the new
	// worktree (vendor, node_modules, …). Glob meta-chars expand
	// against repoRoot. Idempotent: existing dst is skipped.
	if err := BringInFiles(req.RepoRoot, wtPath, cfg.Worktrees.Links, "link", sink); err != nil {
		return CreateResult{}, err
	}
	// worktrees.copies — per-worktree copies so patches can mutate
	// per-branch without affecting main.
	if err := BringInFiles(req.RepoRoot, wtPath, cfg.Worktrees.Copies, "copy", sink); err != nil {
		return CreateResult{}, err
	}

	sl := slug.For(wtPath, req.Branch)

	// Open the store BEFORE patching so we can register the worktree
	// row and allocate ports — the ports map has to flow into the
	// template context before patch render so `{port_<name>}` tokens
	// resolve to their freshly assigned values.
	reg, err := registerAndAllocate(ctx, req, &cfg, wtPath, sl)
	if err != nil {
		return CreateResult{}, err
	}
	defer func() { _ = reg.st.Close() }()
	tplCtx := template.FromSlug(sl).WithPorts(reg.portMap)

	if err := applyPatches(ctx, cfg.Patches, wtPath, tplCtx, sink); err != nil {
		return CreateResult{}, err
	}

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

	return finishCreate(ctx, req, &cfg, result, wtPath, sink)
}

// addGitWorktree decides the base ref (with an optional pre-fetch),
// runs `git worktree add`, and resolves the worktree path to absolute.
// Mutates *wtPath in place. Extracted from Create as a mechanical lift
// of the git-creation phase.
func addGitWorktree(ctx context.Context, req CreateRequest, wtPath *string) error {
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
	// shell idiom — `cd "$(treeman wt create x --print-path)"` —
	// doesn't ingest "Preparing worktree …" / "HEAD is now at …"
	// lines that git emits on its stdout.
	if err := gitcmd.RunPiped(ctx, req.RepoRoot, os.Stderr, os.Stderr, gitArgs...); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	if abs, err := filepath.Abs(*wtPath); err == nil {
		*wtPath = abs
	}
	return nil
}

// registration bundles the store handle and the derived ids/ports a
// Create call needs after the worktree row is registered.
type registration struct {
	st      *store.Store
	repoID  int64
	wtID    int64
	allocs  []ports.Allocation
	portMap map[string]uint16
}

// registerAndAllocate opens the store, registers the repo + worktree
// rows, and allocates the configured ports. The caller owns reg.st and
// must Close it. Extracted from Create as a mechanical lift of the
// registration phase.
func registerAndAllocate(ctx context.Context, req CreateRequest, cfg *config.Config, wtPath string, sl slug.Slug) (registration, error) {
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return registration{}, err
	}
	repoID, err := st.EnsureRepo(ctx, req.RepoRoot, filepath.Base(req.RepoRoot))
	if err != nil {
		_ = st.Close()
		return registration{}, err
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, req.Branch)
	if err != nil {
		_ = st.Close()
		return registration{}, err
	}
	allocs, err := ports.New().Allocate(ctx, st, cfg, repoID, wtID)
	if err != nil {
		_ = st.Close()
		return registration{}, fmt.Errorf("port allocation: %w", err)
	}
	portMap := map[string]uint16{}
	for _, a := range allocs {
		portMap[a.Name] = a.Port
	}
	return registration{st: st, repoID: repoID, wtID: wtID, allocs: allocs, portMap: portMap}, nil
}

// finishCreate resolves the terminal CreateStatus: no-finalize when
// hooks are skipped or nothing needs running, queued when the daemon
// accepts the finalize dispatch, or detached when it's unreachable.
// Extracted from Create as a mechanical lift of the dispatch phase.
func finishCreate(
	ctx context.Context,
	req CreateRequest,
	cfg *config.Config,
	result CreateResult,
	wtPath string,
	sink Sink,
) (CreateResult, error) {
	if req.SkipHooks {
		result.Status = CreatedNoFinalize
		return result, nil
	}

	needsWork := len(cfg.Hooks.OnCreateBeforeEngines) > 0 ||
		len(cfg.Hooks.OnCreateAfterEngines) > 0 ||
		(!req.SkipPrepare && len(cfg.Databases) > 0)
	if !needsWork {
		result.Status = CreatedNoFinalize
		return result, nil
	}

	// Three dispatch paths in priority order:
	//   1. Daemon RPC (the normal happy path).
	//   2. Daemon RPC after ensureDaemon (cold-start).
	//   3. Detach a setsid child running `treeman wt finalize --local`.
	if queued := DispatchFinalize(ctx, req.RepoRoot, wtPath, req.Env, sink); queued {
		result.Status = CreatedQueued
		return result, nil
	}
	logPath, err := DetachFinalize(wtPath, req.RepoRoot)
	if err != nil {
		return result, fmt.Errorf("detach finalize: %w", err)
	}
	sink.OK("queued: setup + prepare detached (daemon unreachable — log: %s)", logPath)
	result.Status = CreatedDetached
	result.LogPath = logPath
	return result, nil
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
