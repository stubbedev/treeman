package wt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/patcher"
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
	WtPath     string       `json:"wt_path"`
	Slug       string       `json:"slug"`
	RepoID     int64        `json:"repo_id"`
	WorktreeID int64        `json:"worktree_id"`
	Status     CreateStatus `json:"status"`
	LogPath    string       `json:"log_path,omitempty"`
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
		return CreateResult{}, fmt.Errorf("branch is required")
	}
	if req.RepoRoot == "" {
		return CreateResult{}, fmt.Errorf("repo_root is required")
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
		gitArgs = []string{"worktree", "add", wtPath, req.Branch}
	} else {
		gitArgs = []string{"worktree", "add", "-b", req.Branch, wtPath, base}
	}
	// Route git's output to stderr (not stdout) so the --print-path
	// shell idiom — `cd "$(treeman wt create x --print-path)"` —
	// doesn't ingest "Preparing worktree …" / "HEAD is now at …"
	// lines that git emits on its stdout.
	if err := gitcmd.RunPiped(ctx, req.RepoRoot, os.Stderr, os.Stderr, gitArgs...); err != nil {
		return CreateResult{}, fmt.Errorf("git worktree add: %w", err)
	}
	abs, err := filepath.Abs(wtPath)
	if err == nil {
		wtPath = abs
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
	tplCtx := template.FromSlug(sl)

	// Top-level patches: dotenv / phpunit.xml / yaml / json rewrites.
	for _, p := range cfg.Patches {
		res, err := patcher.Apply(p, wtPath, tplCtx)
		if err != nil {
			return CreateResult{}, err
		}
		if res.Outcome == patcher.Updated {
			sink.Info("patched %s (%s)", filepath.Join(wtPath, res.File), res.Driver)
		}
	}

	// Register in SQLite.
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return CreateResult{}, err
	}
	defer st.Close()
	repoID, err := st.EnsureRepo(ctx, req.RepoRoot, filepath.Base(req.RepoRoot))
	if err != nil {
		return CreateResult{}, err
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, req.Branch)
	if err != nil {
		return CreateResult{}, err
	}
	sink.OK("created worktree #%d slug=%s path=%s", wtID, sl.Value, wtPath)

	result := CreateResult{
		WtPath:     wtPath,
		Slug:       sl.Value,
		RepoID:     repoID,
		WorktreeID: wtID,
	}

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
