package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemonctl"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/initgen"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/schema"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/wt"
	"github.com/stubbedev/treeman/internal/wtreg"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// registerWriteTools binds every tool that mutates state, including
// the shell-spawning `treeman wt create` / `wt delete` wrappers.
// No flag-gating: the MCP surface is the fully-qualified link to
// treeman's functionality; clients restrict at the agent-policy
// layer.
func registerWriteTools(srv *mcpsdk.Server) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "prepare_run",
		Description: "Drive the full prepare pipeline for a worktree (ensure source DB → load dump → run migrate → run seed → snapshot → fanout test clones). Foreground — BLOCKS until every engine returns. Long-running on cold-builds; pair with logs_wait if you need to surface progress to the user while it runs. Use prepare_run when a user-driven schema or seed change needs to propagate; the daemon's watcher already re-runs this on input edits.",
		Annotations: writeAnno("Run prepare", true, true, true),
	}, prepareTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "hook_run",
		Description: "Execute one configured hook phase (setup|teardown) synchronously for a worktree. Returns per-group exit codes and stdout/stderr tails. Use this to re-run a flaky setup phase without recreating the worktree.",
		Annotations: writeAnno("Run hook phase", true, false, true),
	}, hookTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_write",
		Description: "Overwrite .treeman.yaml with the supplied body. Parses the body into config.Config FIRST and only writes if parsing succeeds — invalid YAML never lands on disk. Preview the diff with config_diff before calling this. Returns the byte count written.",
		Annotations: writeAnno("Write .treeman.yaml", true, true, false),
	}, configWriteTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_set",
		Description: "Patch ONE field of .treeman.yaml by dotted path (e.g. 'daemon.gc_interval', 'databases[0].engine'). Preserves surrounding comments + key ordering by editing the YAML AST in place — prefer this over config_write for surgical edits. Creates missing intermediate mapping keys; refuses to extend sequences. The result is validated before the write lands. Returns previous + new value as JSON.",
		Annotations: writeAnno("Patch config field", false, true, false),
	}, configSetTool)

	// In-process registry mutations. No shell-out, no daemon dependency
	// — agents can reconcile drift the same way `treeman wt
	// register|unregister` would.
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "registry_register",
		Description: "Add a worktree row to the SQLite registry without touching git. Use this when a worktree exists on disk but treeman doesn't know about it (typically: created via raw `git worktree add`). Slug is auto-computed when omitted. Idempotent — re-registering the same path updates the row. Returns the upserted row id.",
		Annotations: writeAnno("Register worktree", false, true, false),
	}, registryRegisterTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "registry_unregister",
		Description: "Mark a worktree deleted in SQLite without touching git or external resources (databases stay, on-disk path stays). Use this when the on-disk worktree was removed externally and the SQLite row needs to follow. Idempotent. Resolves name by slug, branch, or basename.",
		Annotations: writeAnno("Unregister worktree", true, true, false),
	}, registryUnregisterTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "registry_repair",
		Description: "Reconcile the SQLite registry with `git worktree list`. Registers worktrees git knows that SQLite doesn't and marks deleted those SQLite knows that git doesn't. Use this when registry_register/unregister would be the wrong tool because you don't know which direction the drift is in. Returns per-action counts.",
		Annotations: writeAnno("Repair registry", true, true, true),
	}, registryRepairTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "registry_remove",
		Description: "Drop a REPO from the SQLite registry. Stops the daemon's watchers attached to the repo and cascades to delete child rows (worktrees, events, snapshots, hook_runs). External resources (databases, on-disk worktrees, dump caches) are NOT touched. Refuses by default when active worktrees still exist — pass force=true to override.",
		Annotations: writeAnno("Remove repo", true, true, false),
	}, registryRemoveTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "snapshots_purge",
		Description: "DELETE every cached snapshot (template DB) belonging to one repo. Frees engine-side storage and forces the next prepare to cold-build from scratch. Use snapshots_list FIRST to see what will be wiped. For targeted eviction of only stale entries, use the cache-cleanup prompt instead. Returns counts + per-engine errors.",
		Annotations: writeAnno("Purge snapshots", true, true, true),
	}, snapshotsPurgeTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "logs_purge",
		Description: "Delete event-log rows. Filters are AND-combined; pass older_than=24h to drop anything older. At least one filter is REQUIRED to prevent accidental full wipes. Returns the row count removed.",
		Annotations: writeAnno("Purge events", true, false, false),
	}, logsPurgeTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "schema_install",
		Description: "Generate the JSON Schema for .treeman.yaml and wire it into the yaml-language-server modeline so editor autocomplete + inline validation work. target=repo writes <repo>/schemas/treeman.schema.json (default). target=global writes a user-XDG path shared across repos. target=url skips the file write and points the modeline at the canonical upstream URL.",
		Annotations: writeAnno("Install schema", false, true, false),
	}, schemaInstallTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "init_repo",
		Description: "Scaffold a fresh .treeman.yaml under cwd (or --repo). Auto-detects migration framework + JS package manager and emits matching databases/hooks blocks. Use the scaffold-from-framework prompt for the full guided flow. Pass force=true to overwrite an existing file. Returns the chosen path, byte count, and detected framework names.",
		Annotations: writeAnno("Scaffold config", false, false, false),
	}, initRepoTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "daemon_control",
		Description: "Start or stop treemand. action ∈ {start, stop}. Prefers the installed systemd/launchd unit when present; otherwise forks the treemand binary (start) or sends the shutdown RPC (stop). Use this only when daemon_status reports not-running and you need it back up.",
		Annotations: writeAnno("Daemon control", true, true, true),
	}, daemonControlTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_create",
		Description: "Create a new git worktree under .worktrees/<branch> AND dispatch setup hooks + prepare via the daemon. Use branches_list first to pick an unoccupied branch. Returns structured result: wt_path, slug, repo_id, worktree_id, status (queued|detached|noop|no_finalize), log_path (when detached). Non-blocking — the heavy tail runs in the daemon (or a detached child when the daemon is unreachable); tail progress via logs_tail.",
		Annotations: writeAnno("Create worktree", false, false, true),
	}, worktreeCreateTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_delete",
		Description: "Tear down a worktree end-to-end: run teardown hooks → drop databases/redis prefixes/ES indices → remove the git worktree directory. Use worktree_show first to confirm the right slug. Returns structured result: wt_path, status (queued|detached), log_path (when detached). Non-blocking — teardown runs in the daemon.",
		Annotations: writeAnno("Delete worktree", true, true, true),
	}, worktreeDeleteTool)

	registerEngineWriteTools(srv)
}

// ─── prepare_run ──────────────────────────────────────────────────

type prepareIn struct {
	Worktree string `json:"worktree,omitempty" jsonschema:"defaults to cwd"`
	Repo     string `json:"repo,omitempty"`
}
type prepareOut struct {
	Outcomes []prepare.Outcome `json:"outcomes"`
}

func prepareTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in prepareIn) (*mcpsdk.CallToolResult, prepareOut, error) {
	outs, err := runPrepare(ctx, in.Worktree, in.Repo)
	if err != nil {
		return nil, prepareOut{}, err
	}
	return nil, prepareOut{Outcomes: outs}, nil
}

// ─── hook_run ─────────────────────────────────────────────────────

type hookIn struct {
	Phase    string `json:"phase" jsonschema:"setup|teardown"`
	Worktree string `json:"worktree,omitempty"`
}
type hookOut struct {
	Phase   string           `json:"phase"`
	Outcome hooks.RunOutcome `json:"outcome"`
}

func hookTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in hookIn) (*mcpsdk.CallToolResult, hookOut, error) {
	out, err := runHookPhase(ctx, in.Phase, in.Worktree)
	if err != nil {
		return nil, hookOut{}, err
	}
	return nil, hookOut{Phase: in.Phase, Outcome: out}, nil
}

// ─── config_write ─────────────────────────────────────────────────

type configWriteIn struct {
	Repo string `json:"repo,omitempty"`
	Body string `json:"body" jsonschema:"the full YAML body to write to .treeman.yaml"`
}
type configWriteOut struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

func configWriteTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configWriteIn) (*mcpsdk.CallToolResult, configWriteOut, error) {
	if in.Body == "" {
		return nil, configWriteOut{}, fmt.Errorf("body is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, configWriteOut{}, err
	}
	// Validate by parsing before any write so a malformed body
	// never overwrites a working config.
	var parsed config.Config
	if err := yaml.Unmarshal([]byte(in.Body), &parsed); err != nil {
		return nil, configWriteOut{}, fmt.Errorf("yaml parse: %w", err)
	}
	target := filepath.Join(repoRoot, ".treeman.yaml")
	if err := atomicWrite(target, []byte(in.Body)); err != nil {
		return nil, configWriteOut{}, err
	}
	writeMCPEvent(context.Background(), "config_write", "replaced .treeman.yaml", 0, map[string]string{
		"repo":  repoRoot,
		"bytes": fmt.Sprintf("%d", len(in.Body)),
	})
	return nil, configWriteOut{Path: target, Bytes: len(in.Body)}, nil
}

// atomicWrite delegates to yamlpatch.AtomicWriteWithBackup so MCP
// mutations (config_write, config_set) leave behind a rotated
// `.treeman.yaml.bak.*` history. Five-snapshot cap matches the CLI.
func atomicWrite(path string, data []byte) error {
	return yamlpatch.AtomicWriteWithBackup(path, data, 5)
}

// ─── init_repo / schema_install ───────────────────────────────────

type initIn struct {
	Repo  string `json:"repo,omitempty"`
	Force bool   `json:"force,omitempty"`
}
type initOut struct {
	Path     string   `json:"path"`
	Created  bool     `json:"created"`
	Bytes    int      `json:"bytes"`
	Detected []string `json:"detected"`
}

func initRepoTool(_ context.Context, _ *mcpsdk.CallToolRequest, in initIn) (*mcpsdk.CallToolResult, initOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, initOut{}, fmt.Errorf("resolve repo: %w", err)
	}
	target, created, body, err := initgen.WriteYAML(repoRoot, in.Force)
	if err != nil {
		return nil, initOut{}, err
	}
	if created {
		writeMCPEvent(context.Background(), "init_repo", "scaffolded "+target, 0, map[string]string{
			"repo": repoRoot,
			"path": target,
		})
	}
	return nil, initOut{
		Path:     target,
		Created:  created,
		Bytes:    len(body),
		Detected: initgen.DetectFrameworkNames(repoRoot),
	}, nil
}

type schemaInstallIn struct {
	Repo   string `json:"repo,omitempty"`
	Target string `json:"target,omitempty" jsonschema:"repo|global|url; default repo"`
}
type schemaInstallOut struct {
	Target          string `json:"target"`
	Resolved        string `json:"resolved"`
	ModelineChanged bool   `json:"modeline_changed"`
	WroteFile       bool   `json:"wrote_file"`
}

func schemaInstallTool(_ context.Context, _ *mcpsdk.CallToolRequest, in schemaInstallIn) (*mcpsdk.CallToolResult, schemaInstallOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, schemaInstallOut{}, err
	}
	var target schema.Target
	switch strings.ToLower(in.Target) {
	case "", "repo":
		target = schema.TargetRepo
	case "global":
		target = schema.TargetGlobal
	case "url":
		target = schema.TargetURL
	default:
		return nil, schemaInstallOut{}, fmt.Errorf("invalid target %q (want repo|global|url)", in.Target)
	}
	resolved, changed, err := schema.Install(repoRoot, target)
	if err != nil {
		return nil, schemaInstallOut{}, err
	}
	writeMCPEvent(context.Background(), "schema_install", "installed "+resolved, 0, map[string]string{
		"repo":     repoRoot,
		"target":   strings.ToLower(in.Target),
		"resolved": resolved,
	})
	return nil, schemaInstallOut{
		Target:          strings.ToLower(in.Target),
		Resolved:        resolved,
		ModelineChanged: changed,
		WroteFile:       target != schema.TargetURL,
	}, nil
}

// ─── registry mutations ───────────────────────────────────────────

type registryRegisterIn struct {
	Repo   string `json:"repo,omitempty"`
	Path   string `json:"path" jsonschema:"absolute worktree path"`
	Branch string `json:"branch,omitempty" jsonschema:"branch name; derived from .git/HEAD when omitted"`
}
type registryRegisterOut struct {
	WorktreeID int64  `json:"worktree_id"`
	RepoID     int64  `json:"repo_id"`
	Slug       string `json:"slug"`
	Branch     string `json:"branch,omitempty"`
	Path       string `json:"path"`
}

func registryRegisterTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in registryRegisterIn) (*mcpsdk.CallToolResult, registryRegisterOut, error) {
	if in.Path == "" {
		return nil, registryRegisterOut{}, fmt.Errorf("path is required")
	}
	wt, err := filepath.Abs(in.Path)
	if err != nil {
		return nil, registryRegisterOut{}, fmt.Errorf("abs %s: %w", in.Path, err)
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, registryRegisterOut{}, err
	}
	branch := in.Branch
	if branch == "" {
		branch = detectBranch(wt)
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, registryRegisterOut{}, err
	}
	defer st.Close()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return nil, registryRegisterOut{}, fmt.Errorf("ensure repo: %w", err)
	}
	sl := slug.For(wt, branch)
	wtID, err := st.EnsureWorktree(ctx, repoID, wt, sl.Value, branch)
	if err != nil {
		return nil, registryRegisterOut{}, fmt.Errorf("ensure worktree: %w", err)
	}
	writeMCPEvent(context.Background(), "registry_register", "registered "+wt, repoID, map[string]string{
		"path":   wt,
		"slug":   sl.Value,
		"branch": branch,
	})
	return nil, registryRegisterOut{
		WorktreeID: wtID,
		RepoID:     repoID,
		Slug:       sl.Value,
		Branch:     branch,
		Path:       wt,
	}, nil
}

type registryUnregisterIn struct {
	Repo string `json:"repo,omitempty"`
	Name string `json:"name" jsonschema:"slug, branch, or basename of the worktree"`
}
type registryUnregisterOut struct {
	WorktreeID int64  `json:"worktree_id"`
	Path       string `json:"path"`
}

func registryUnregisterTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in registryUnregisterIn) (*mcpsdk.CallToolResult, registryUnregisterOut, error) {
	if in.Name == "" {
		return nil, registryUnregisterOut{}, fmt.Errorf("name is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, registryUnregisterOut{}, err
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, registryUnregisterOut{}, err
	}
	defer st.Close()
	repoID, err := lookupRepoID(ctx, st, repoRoot)
	if err != nil {
		return nil, registryUnregisterOut{}, fmt.Errorf("lookup repo %s: %w", repoRoot, err)
	}
	wtID, _ := st.LookupWorktreeID(ctx, repoID, in.Name)
	if wtID == 0 {
		return nil, registryUnregisterOut{}, fmt.Errorf("no worktree matches %q", in.Name)
	}
	var path string
	_ = st.DB.QueryRowContext(ctx, `SELECT path FROM worktrees WHERE id = ?`, wtID).Scan(&path)
	if err := st.MarkWorktreeDeleted(ctx, wtID); err != nil {
		return nil, registryUnregisterOut{}, err
	}
	writeMCPEvent(context.Background(), "registry_unregister", "unregistered "+path, repoID, map[string]string{
		"path": path,
		"name": in.Name,
	})
	return nil, registryUnregisterOut{WorktreeID: wtID, Path: path}, nil
}

type registryRemoveIn struct {
	Repo  string `json:"repo,omitempty"`
	Force bool   `json:"force,omitempty" jsonschema:"remove even when active worktrees exist"`
}
type registryRemoveOut struct {
	RepoPath string `json:"repo"`
	Via      string `json:"via" jsonschema:"daemon|sqlite — which path performed the delete"`
}

func registryRemoveTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in registryRemoveIn) (*mcpsdk.CallToolResult, registryRemoveOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, registryRemoveOut{}, err
	}
	// Prefer the daemon RPC so live watchers stop in-process; fall
	// back to direct SQLite when the daemon isn't running.
	if sock, _ := rpc.SocketPath(); sock != "" {
		if _, statErr := os.Stat(sock); statErr == nil {
			resp, callErr := rpc.Call(ctx, rpc.Request{
				Method:     rpc.MethodRepoRemove,
				RepoRemove: &rpc.RepoRemoveArgs{RepoPath: repoRoot, Force: in.Force},
			})
			if callErr == nil {
				if resp.Kind == rpc.KindError {
					return nil, registryRemoveOut{}, fmt.Errorf("daemon: %s", resp.Message)
				}
				writeMCPEvent(context.Background(), "registry_remove", "removed "+repoRoot, 0, map[string]string{
					"repo": repoRoot,
					"via":  "daemon",
				})
				return nil, registryRemoveOut{RepoPath: repoRoot, Via: "daemon"}, nil
			}
		}
	}

	st, err := openStore(ctx)
	if err != nil {
		return nil, registryRemoveOut{}, err
	}
	defer st.Close()
	repoID, err := st.LookupRepoID(ctx, repoRoot)
	if err != nil {
		return nil, registryRemoveOut{}, err
	}
	if repoID == 0 {
		return nil, registryRemoveOut{}, fmt.Errorf("repo not enrolled: %s", repoRoot)
	}
	if !in.Force {
		n, err := st.CountActiveWorktreesForRepo(ctx, repoID)
		if err != nil {
			return nil, registryRemoveOut{}, err
		}
		if n > 0 {
			return nil, registryRemoveOut{}, fmt.Errorf("repo has %d active worktree(s); pass force=true to override", n)
		}
	}
	if err := st.RemoveRepo(ctx, repoID); err != nil {
		return nil, registryRemoveOut{}, err
	}
	writeMCPEvent(context.Background(), "registry_remove", "removed "+repoRoot, 0, map[string]string{
		"repo": repoRoot,
		"via":  "sqlite",
	})
	return nil, registryRemoveOut{RepoPath: repoRoot, Via: "sqlite"}, nil
}

type registryRepairIn struct {
	Repo string `json:"repo,omitempty"`
}

func registryRepairTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in registryRepairIn) (*mcpsdk.CallToolResult, wtreg.RepairResult, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, wtreg.RepairResult{}, err
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, wtreg.RepairResult{}, err
	}
	defer st.Close()
	res, err := wtreg.Repair(ctx, st, repoRoot, detectBranch)
	if err == nil && (len(res.Registered) > 0 || len(res.Unregistered) > 0) {
		writeMCPEvent(context.Background(), "registry_repair",
			fmt.Sprintf("repaired %d registered, %d unregistered", len(res.Registered), len(res.Unregistered)),
			0, map[string]string{"repo": repoRoot})
	}
	return nil, res, err
}

// ─── snapshots_purge ──────────────────────────────────────────────

type snapshotsPurgeIn struct {
	Repo string `json:"repo,omitempty"`
}
type snapshotsPurgeOut struct {
	Repo    string   `json:"repo"`
	Dropped int      `json:"dropped"`
	Errors  []string `json:"errors,omitempty"`
}

func snapshotsPurgeTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in snapshotsPurgeIn) (*mcpsdk.CallToolResult, snapshotsPurgeOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, snapshotsPurgeOut{}, err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, snapshotsPurgeOut{}, fmt.Errorf("load config: %w", err)
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, snapshotsPurgeOut{}, err
	}
	defer st.Close()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return nil, snapshotsPurgeOut{}, fmt.Errorf("ensure repo: %w", err)
	}
	dropped, errs := snapshot.PurgeRepo(ctx, &cfg, st, repoID)
	writeMCPEvent(context.Background(), "snapshots_purge",
		fmt.Sprintf("purged %d snapshot(s)", dropped), repoID,
		map[string]string{"repo": repoRoot, "dropped": fmt.Sprintf("%d", dropped)})
	out := snapshotsPurgeOut{Repo: repoRoot, Dropped: dropped}
	for _, e := range errs {
		out.Errors = append(out.Errors, e.Error())
	}
	return nil, out, nil
}

// ─── logs_purge ───────────────────────────────────────────────────

type logsPurgeIn struct {
	Repo       string   `json:"repo,omitempty"`
	Worktree   string   `json:"worktree,omitempty" jsonschema:"slug, branch, or basename"`
	OlderThan  string   `json:"older_than,omitempty" jsonschema:"duration (24h, 7d) or RFC3339 cutoff — events older than this are purged"`
	Levels     []string `json:"levels,omitempty"`
	EventTypes []string `json:"event_types,omitempty"`
}
type logsPurgeOut struct {
	RowsRemoved int64 `json:"rows_removed"`
}

func logsPurgeTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in logsPurgeIn) (*mcpsdk.CallToolResult, logsPurgeOut, error) {
	// At least one filter required so an empty input can never wipe
	// the entire events table.
	if in.Repo == "" && in.Worktree == "" && in.OlderThan == "" && len(in.Levels) == 0 && len(in.EventTypes) == 0 {
		return nil, logsPurgeOut{}, errors.New("at least one filter (repo, worktree, older_than, levels, event_types) is required")
	}
	f := store.EventFilter{
		Levels:     validateLevels(in.Levels),
		EventTypes: in.EventTypes,
	}
	if in.OlderThan != "" {
		t, err := parsePurgeCutoff(in.OlderThan)
		if err != nil {
			return nil, logsPurgeOut{}, err
		}
		f.UntilMs = t.UnixMilli()
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, logsPurgeOut{}, err
	}
	defer st.Close()
	if in.Repo != "" || in.Worktree != "" {
		repoRoot, err := resolveRepo(in.Repo)
		if err == nil && repoRoot != "" {
			if rid, err := lookupRepoID(ctx, st, repoRoot); err == nil {
				f.RepoID = rid
			}
		}
		if in.Worktree != "" {
			wid, _ := st.LookupWorktreeID(ctx, f.RepoID, in.Worktree)
			if wid == 0 {
				return nil, logsPurgeOut{}, fmt.Errorf("no worktree matches %q", in.Worktree)
			}
			f.WorktreeID = wid
		}
	}
	n, err := st.PurgeEvents(ctx, f)
	if err != nil {
		return nil, logsPurgeOut{}, err
	}
	writeMCPEvent(context.Background(), "logs_purge",
		fmt.Sprintf("purged %d event row(s)", n), f.RepoID,
		map[string]string{"rows_removed": fmt.Sprintf("%d", n)})
	return nil, logsPurgeOut{RowsRemoved: n}, nil
}

// parsePurgeCutoff accepts "24h" / "7d" / RFC3339 timestamps. For
// durations the cutoff is interpreted as "older than now-d"; for an
// absolute timestamp it's the timestamp itself.
func parsePurgeCutoff(s string) (time.Time, error) {
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		d, err := time.ParseDuration(days + "h")
		if err == nil {
			return time.Now().Add(-d * 24), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	for _, f := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised older_than %q", s)
}

// ─── worktree_create / delete ─────────────────────────────────────

type worktreeCreateIn struct {
	Branch  string `json:"branch" jsonschema:"branch name for the new worktree"`
	From    string `json:"from,omitempty" jsonschema:"base branch"`
	Path    string `json:"path,omitempty" jsonschema:"explicit worktree path"`
	Repo    string `json:"repo,omitempty"`
	NoFetch bool   `json:"no_fetch,omitempty"`
}

func worktreeCreateTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeCreateIn) (*mcpsdk.CallToolResult, wt.CreateResult, error) {
	if in.Branch == "" {
		return nil, wt.CreateResult{}, fmt.Errorf("branch is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, wt.CreateResult{}, fmt.Errorf("resolve repo: %w", err)
	}
	res, err := wt.Create(ctx, wt.CreateRequest{
		RepoRoot: repoRoot,
		Branch:   in.Branch,
		From:     in.From,
		Path:     in.Path,
		NoFetch:  in.NoFetch,
	}, wt.NoopSink{})
	return nil, res, err
}

type worktreeDeleteIn struct {
	Name  string `json:"name" jsonschema:"slug, branch, or basename of the worktree to delete"`
	Repo  string `json:"repo,omitempty"`
	Force bool   `json:"force,omitempty"`
}

func worktreeDeleteTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeDeleteIn) (*mcpsdk.CallToolResult, wt.DeleteResult, error) {
	if in.Name == "" {
		return nil, wt.DeleteResult{}, fmt.Errorf("name is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, wt.DeleteResult{}, fmt.Errorf("resolve repo: %w", err)
	}
	res, err := wt.Delete(ctx, wt.DeleteRequest{
		RepoRoot: repoRoot,
		Target:   in.Name,
		Force:    in.Force,
	}, wt.NoopSink{})
	return nil, res, err
}

// ─── daemon_control ───────────────────────────────────────────────

type daemonControlIn struct {
	Action string `json:"action" jsonschema:"start|stop"`
}
type daemonControlOut struct {
	Action string `json:"action"`
	PID    int    `json:"pid,omitempty"`
}

func daemonControlTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in daemonControlIn) (*mcpsdk.CallToolResult, daemonControlOut, error) {
	switch in.Action {
	case "start":
		pid, err := daemonctl.Start(ctx)
		if err != nil {
			return nil, daemonControlOut{}, err
		}
		return nil, daemonControlOut{Action: "start", PID: pid}, nil
	case "stop":
		if err := daemonctl.Stop(ctx); err != nil {
			return nil, daemonControlOut{}, err
		}
		return nil, daemonControlOut{Action: "stop"}, nil
	default:
		return nil, daemonControlOut{}, fmt.Errorf("action must be start or stop, got %q", in.Action)
	}
}

// ─── config_set ───────────────────────────────────────────────────

type configSetIn struct {
	Repo  string `json:"repo,omitempty"`
	Path  string `json:"path" jsonschema:"dotted path like 'daemon.gc_interval' or 'databases[0].engine'"`
	Value any    `json:"value" jsonschema:"the value to set (scalar, array, or object). Pass null to clear."`
}
type configSetOut struct {
	Path         string `json:"path"`
	PreviousJSON string `json:"previous_json,omitempty"`
	NewJSON      string `json:"new_json"`
	Bytes        int    `json:"bytes"`
}

func configSetTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configSetIn) (*mcpsdk.CallToolResult, configSetOut, error) {
	if in.Path == "" {
		return nil, configSetOut{}, fmt.Errorf("path is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, configSetOut{}, err
	}
	p := filepath.Join(repoRoot, ".treeman.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, configSetOut{}, fmt.Errorf("read %s: %w", p, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, configSetOut{}, fmt.Errorf("parse %s: %w", p, err)
	}
	segs, err := yamlpatch.ParsePath(in.Path)
	if err != nil {
		return nil, configSetOut{}, err
	}
	newNode, err := yamlpatch.ValueToNode(in.Value)
	if err != nil {
		return nil, configSetOut{}, fmt.Errorf("encode value: %w", err)
	}
	prev, err := yamlpatch.Set(&doc, segs, newNode)
	if err != nil {
		return nil, configSetOut{}, err
	}
	body, err := yamlpatch.Marshal(&doc)
	if err != nil {
		return nil, configSetOut{}, fmt.Errorf("encode yaml: %w", err)
	}
	var validated config.Config
	if err := yaml.Unmarshal(body, &validated); err != nil {
		return nil, configSetOut{}, fmt.Errorf("validation failed — patched file would not parse as config.Config: %w", err)
	}
	if err := atomicWrite(p, body); err != nil {
		return nil, configSetOut{}, err
	}
	prevJSON := ""
	if prev != nil {
		var prevVal any
		if err := prev.Decode(&prevVal); err == nil {
			if b, err := json.Marshal(prevVal); err == nil {
				prevJSON = string(b)
			}
		}
	}
	newJSON, _ := json.Marshal(in.Value)
	writeMCPEvent(context.Background(), "config_set", "patched "+in.Path, 0, map[string]string{
		"repo":     repoRoot,
		"path":     in.Path,
		"new_json": string(newJSON),
	})
	return nil, configSetOut{
		Path:         in.Path,
		PreviousJSON: prevJSON,
		NewJSON:      string(newJSON),
		Bytes:        len(body),
	}, nil
}
