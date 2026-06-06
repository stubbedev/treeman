package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemonctl"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/initgen"
	"github.com/stubbedev/treeman/internal/ports"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/schema"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
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
	addTool(srv, &mcpsdk.Tool{
		Name:        "prepare_run",
		Description: "Run the full prepare pipeline for a worktree (ensure source DB → dump → migrate → seed → snapshot → fanout clones). BLOCKS until every engine returns; long on cold builds — pair with logs_wait to stream progress. The daemon's watcher already re-runs this on input edits; call manually only when an out-of-band schema/seed change must propagate.",
		Annotations: writeAnno("Run prepare", true, true, true),
	}, prepareTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "db_reset",
		Description: "Reset a worktree's branch_scoped DBs to the base branch's data — drops the active namespace + current branch's durable copy, then re-seeds from the parent. DESTRUCTIVE for the current branch's working data (other branches' durable copies kept). Pass dry_run=true to preview which namespaces would be dropped. No-op when no DBs are branch_scoped.",
		Annotations: writeAnno("Reset branch_scoped DBs", true, true, true),
	}, dbResetTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "hook_run",
		Description: "Re-run one configured hook phase synchronously for a worktree. Optional env_overrides lets you tweak a var (e.g. DEBUG=1) for THIS run without editing .treeman.yaml. Returns per-group exit codes + stdout/stderr tails.",
		Annotations: writeAnno("Run hook phase", true, false, true),
	}, hookTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "config_write",
		Description: "Overwrite a config with a full body. scope=repo (default — .treeman.yaml) | global (~/.config/treeman/config.yaml). Always preview with config_diff first. Parses + scope-checks body before any write — invalid YAML or a misplaced-layer key never lands on disk. For surgical one-field edits prefer config_set.",
		Annotations: writeAnno("Write config", true, true, false),
	}, configWriteTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "config_set",
		Description: "Patch one config field by dotted path (e.g. 'daemon.gc_interval', 'databases[0].engine'). scope=repo (default — .treeman.yaml) | global (~/.config/treeman/config.yaml). Preserves comments + key order. Refuses to extend sequences. Scope-checks the top-level key + validates before the write. Creates the file if missing. Prefer over config_write for any single-field change.",
		Annotations: writeAnno("Patch config field", false, true, false),
	}, configSetTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "config_unset",
		Description: "Delete one key (or sequence element) from a config by dotted path (e.g. 'daemon.gc_interval', 'databases[0]'). scope=repo (default) | global. Drops the key entirely — config_set with null only nulls it. Preserves comments + order, snapshots prior content to SQLite (recoverable via config_restore), validates before the write.",
		Annotations: writeAnno("Remove config field", true, true, false),
	}, configUnsetTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "config_delete",
		Description: "Delete a whole config FILE from disk. scope=repo (default — .treeman.yaml) | global (~/.config/treeman/config.yaml). DESTRUCTIVE but recoverable: content is snapshotted to SQLite first (config_restore brings it back). Pass dry_run=true to preview; requires ack=true to actually delete (a bare call only previews).",
		Annotations: writeAnno("Delete config file", true, true, false),
	}, configDeleteTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "config_restore",
		Description: "Restore a stored generation of a config (see config_history) back onto disk. scope=repo (default) | global — must match the scope the generation was recorded under. The current content is snapshotted first, so a restore is itself reversible. Use to roll back a bad config_set/config_write/config_unset/config_delete.",
		Annotations: writeAnno("Restore config generation", true, true, false),
	}, configRestoreTool)

	registerRegistryWriteTools(srv)

	addTool(srv, &mcpsdk.Tool{
		Name:        "repo_remove",
		Description: "Drop a REPO from the registry (cascades to worktrees/events/snapshots/hook_runs). External resources — DBs, on-disk worktrees, dumps — are NOT touched. Pass dry_run=true to count cascaded rows first. Refuses by default if active worktrees exist; pass force=true to override.",
		Annotations: writeAnno("Remove repo", true, true, false),
	}, repoRemoveTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "snapshots_purge",
		Description: "DELETE every cached snapshot for a repo — forces every prepare to cold-build. Pass dry_run=true to preview (or call snapshots_list). For evicting only stale/orphan entries use the cache-cleanup prompt (much safer).",
		Annotations: writeAnno("Purge snapshots", true, true, true),
	}, snapshotsPurgeTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "logs_purge",
		Description: "Delete event-log rows. Filters AND-combine; pass older_than=24h to drop anything older. At least one filter is REQUIRED to prevent a full wipe. Pass dry_run=true to preview the matched-row count; ack=true to skip confirmation.",
		Annotations: writeAnno("Purge events", true, false, false),
	}, logsPurgeTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "schema_install",
		Description: "Generate the .treeman.yaml JSON Schema and wire the yaml-language-server modeline so editors get autocomplete + inline validation. target=repo (default) | global | url.",
		Annotations: writeAnno("Install schema", false, true, false),
	}, schemaInstallTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "init_repo",
		Description: "Scaffold a fresh .treeman.yaml — auto-detects migration framework + JS package manager and emits matching databases/hooks. Pass global=true to instead scaffold the user-global ~/.config/treeman/config.yaml (machine-wide defaults: daemon/snapshots/logs/auto_fetch/notifications) and install its scoped schema. For the full guided flow use the scaffold-from-framework prompt. Pass force=true to overwrite.",
		Annotations: writeAnno("Scaffold config", false, false, false),
	}, initRepoTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "daemon_control",
		Description: "Control treemand. action ∈ {start, stop, reload, install, uninstall}. start/stop prefer the installed systemd/launchd unit (else fork/shutdown-RPC). reload re-reads config + restarts watchers without a process restart (call after a config edit; repo= scopes it). install/uninstall manage the auto-start unit.",
		Annotations: writeAnno("Daemon control", true, true, true),
	}, daemonControlTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "worktree_create",
		Description: "Create a new git worktree under .worktrees/<branch> + dispatch setup hooks + prepare via the daemon. For the full guided flow (branches_list → daemon_status → create → wait) use the worktree-setup prompt. For one-off migration validation use the migration-trial prompt instead. Non-blocking — tail via logs_wait.",
		Annotations: writeAnno("Create worktree", false, false, true),
	}, worktreeCreateTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "worktree_delete",
		Description: "Tear down a worktree end-to-end: teardown hooks → drop DBs/redis prefixes/ES indices → remove the git worktree dir. Pass dry_run=true to preview the per-engine namespaces that would be dropped. Non-blocking — runs in the daemon.",
		Annotations: writeAnno("Delete worktree", true, true, true),
	}, worktreeDeleteTool)

	registerEngineWriteTools(srv)
	registerSyncWriteTools(srv)
}

// registerRegistryWriteTools binds in-process registry mutations. No
// shell-out, no daemon dependency — agents can reconcile drift the same
// way `treeman wt register|unregister` would.
func registerRegistryWriteTools(srv *mcpsdk.Server) {
	addTool(srv, &mcpsdk.Tool{
		Name:        "registry_register",
		Description: "Add a WORKTREE row to SQLite without touching git. Use when a worktree exists on disk (e.g. created via raw `git worktree add`) but treeman doesn't know about it. Idempotent. For drift in an unknown direction use registry_repair.",
		Annotations: writeAnno("Register worktree", false, true, false),
	}, registryRegisterTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "registry_unregister",
		Description: "Mark a WORKTREE deleted in SQLite without touching git or external resources (DBs + on-disk path stay). Use when the on-disk worktree was removed externally. Pass dry_run=true to preview which row would be marked, ack=true to skip confirmation. Idempotent.",
		Annotations: writeAnno("Unregister worktree", true, true, false),
	}, registryUnregisterTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "registry_repair",
		Description: "Reconcile the SQLite worktree registry against `git worktree list` — registers what git knows that SQLite doesn't and marks deleted what SQLite knows that git doesn't. Use when drift direction is unknown.",
		Annotations: writeAnno("Repair registry", true, true, true),
	}, registryRepairTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "worktree_repair",
		Description: "Recover one stuck/broken worktree end-to-end: ensure registry row, ensure ports allocated, dispatch finalize via the daemon (or run prepare inline when the daemon is unreachable), and check each snapshot for orphan templates. Returns one result per action. Idempotent — safe to call when nothing is broken.",
		Annotations: writeAnno("Repair worktree", false, true, true),
	}, worktreeRepairTool)
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

// ─── db_reset ─────────────────────────────────────────────────────

type dbResetIn struct {
	Worktree string `json:"worktree,omitempty" jsonschema:"defaults to cwd's worktree"`
	Repo     string `json:"repo,omitempty"`
	Engine   string `json:"engine,omitempty"   jsonschema:"restrict to one engine family: mysql|postgres|mongodb|redis|elasticsearch (aliases like mariadb/postgresql/valkey/dragonfly accepted); omit to reset all branch_scoped databases"`
	DryRun   bool   `json:"dry_run,omitempty"  jsonschema:"plan only — return the active-namespace + current-branch durable names that WOULD be dropped without performing the reset"`
	Ack      bool   `json:"ack,omitempty"      jsonschema:"skip the elicitation confirmation prompt"`
}
type dbResetOut struct {
	Outcomes  []prepare.Outcome        `json:"outcomes,omitempty"`
	DryRun    bool                     `json:"dry_run,omitempty"`
	WouldDrop []prepare.BranchScopedDB `json:"would_drop,omitempty" jsonschema:"per-database state that would be dropped + re-seeded from the parent branch"`
	Refused   string                   `json:"refused,omitempty"`
}

func dbResetTool(ctx context.Context, req *mcpsdk.CallToolRequest, in dbResetIn) (*mcpsdk.CallToolResult, dbResetOut, error) {
	if in.DryRun {
		dbs, err := runBranchScopedStatus(ctx, in.Worktree, in.Repo)
		if err != nil {
			return nil, dbResetOut{}, err
		}
		filter := strings.ToLower(in.Engine)
		if filter != "" {
			filtered := dbs[:0]
			for _, d := range dbs {
				if strings.EqualFold(d.Engine, filter) {
					filtered = append(filtered, d)
				}
			}
			dbs = filtered
		}
		return nil, dbResetOut{DryRun: true, WouldDrop: dbs}, nil
	}
	if ok, reason := confirmDestructive(ctx, req, false, in.Ack,
		"Reset branch_scoped DBs to the base branch? Current branch's working data will be DROPPED."); !ok {
		return nil, dbResetOut{Refused: reason}, nil
	}
	outs, err := runDbReset(ctx, in.Worktree, in.Repo, strings.ToLower(in.Engine))
	if err != nil {
		return nil, dbResetOut{}, err
	}
	return nil, dbResetOut{Outcomes: outs}, nil
}

// ─── hook_run ─────────────────────────────────────────────────────

type hookIn struct {
	Phase        string            `json:"phase"                   jsonschema:"on-create-before-engines|on-create-after-engines|on-delete-before-engines|on-delete-after-engines|on-checkout"`
	Worktree     string            `json:"worktree,omitempty"`
	EnvOverrides map[string]string `json:"env_overrides,omitempty" jsonschema:"extra env vars merged on top of the captured os.Environ() for this one invocation. Use to retry a flaky hook with a tweaked value without editing .treeman.yaml."`
}
type hookOut struct {
	Phase   string           `json:"phase"`
	Outcome hooks.RunOutcome `json:"outcome"`
}

func hookTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in hookIn) (*mcpsdk.CallToolResult, hookOut, error) {
	out, err := runHookPhase(ctx, in.Phase, in.Worktree, in.EnvOverrides)
	if err != nil {
		return nil, hookOut{}, err
	}
	return nil, hookOut{Phase: in.Phase, Outcome: out}, nil
}

// ─── config_write ─────────────────────────────────────────────────

type configWriteIn struct {
	Repo  string `json:"repo,omitempty"`
	Body  string `json:"body"            jsonschema:"the full YAML body to write"`
	Scope string `json:"scope,omitempty" jsonschema:"repo (default — .treeman.yaml) | global (~/.config/treeman/config.yaml). Scope is enforced: global-only keys in a repo body (or vice-versa) are rejected before the write."`
}
type configWriteOut struct {
	Path  string `json:"path"`
	Scope string `json:"scope"`
	Bytes int    `json:"bytes"`
}

func configWriteTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configWriteIn) (*mcpsdk.CallToolResult, configWriteOut, error) {
	if in.Body == "" {
		return nil, configWriteOut{}, errors.New("body is required")
	}
	target, histRoot, layer, err := resolveConfigTarget(in.Scope, in.Repo)
	if err != nil {
		return nil, configWriteOut{}, err
	}
	// Validate by parsing before any write so a malformed body
	// never overwrites a working config.
	var parsed config.Config
	if err := yaml.Unmarshal([]byte(in.Body), &parsed); err != nil {
		return nil, configWriteOut{}, fmt.Errorf("yaml parse: %w", err)
	}
	// Reject keys that belong in the other layer at write time (hard scope
	// break) — otherwise the body lands and the next load fails.
	if err := config.CheckBodyScope([]byte(in.Body), layer); err != nil {
		return nil, configWriteOut{}, err
	}
	// The global config dir may not exist yet (first write). atomicWrite's
	// tmp+rename needs the parent present; repo dirs always exist.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, configWriteOut{}, fmt.Errorf("create config dir: %w", err)
	}
	if err := atomicWrite(histRoot, target, []byte(in.Body)); err != nil {
		return nil, configWriteOut{}, err
	}
	writeMCPEvent(context.Background(), store.EvtMCPConfigWrite, "replaced "+target, 0, map[string]string{
		"scope": layer,
		"file":  target,
		"bytes": strconv.Itoa(len(in.Body)),
	})
	return nil, configWriteOut{Path: target, Scope: layer, Bytes: len(in.Body)}, nil
}

// atomicWrite stashes the current on-disk content of path as a new
// per-repo config generation in SQLite (best-effort — a store failure
// must not block the edit), then atomically writes the new bytes. MCP
// mutations (config_write, config_set) thus leave a recoverable history
// reachable via `treeman config history|restore`, instead of dropping
// `.treeman.yaml.bak.*` files in the project root.
func atomicWrite(repoRoot, path string, data []byte) error {
	if prev, err := os.ReadFile(path); err == nil {
		ctx := context.Background()
		if st, err := openStore(ctx); err == nil {
			_, _ = st.SnapshotConfig(ctx, repoRoot, path, prev)
			_ = st.Close()
		}
	}
	return yamlpatch.AtomicWrite(path, data)
}

// ─── init_repo / schema_install ───────────────────────────────────

type initIn struct {
	Repo   string `json:"repo,omitempty"`
	Force  bool   `json:"force,omitempty"`
	Global bool   `json:"global,omitempty" jsonschema:"scaffold the user-global ~/.config/treeman/config.yaml (machine-wide defaults) instead of a per-repo .treeman.yaml"`
}
type initOut struct {
	Path     string   `json:"path"`
	Created  bool     `json:"created"`
	Bytes    int      `json:"bytes"`
	Scope    string   `json:"scope"`
	Schema   string   `json:"schema,omitempty"`
	Detected []string `json:"detected,omitempty"`
}

func initRepoTool(_ context.Context, _ *mcpsdk.CallToolRequest, in initIn) (*mcpsdk.CallToolResult, initOut, error) {
	if in.Global {
		target, created, body, err := initgen.WriteGlobalYAML(in.Force)
		if err != nil {
			return nil, initOut{}, err
		}
		schemaPath, _, schemaErr := schema.Install("", schema.TargetGlobal)
		if created {
			writeMCPEvent(context.Background(), store.EvtMCPInitRepo, "scaffolded "+target, 0, map[string]string{
				"path":  target,
				"scope": "global",
			})
		}
		out := initOut{Path: target, Created: created, Bytes: len(body), Scope: "global"}
		if schemaErr == nil {
			out.Schema = schemaPath
		}
		return nil, out, nil
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, initOut{}, fmt.Errorf("resolve repo: %w", err)
	}
	target, created, body, err := initgen.WriteYAML(repoRoot, in.Force)
	if err != nil {
		return nil, initOut{}, err
	}
	if created {
		writeMCPEvent(context.Background(), store.EvtMCPInitRepo, "scaffolded "+target, 0, map[string]string{
			"repo": repoRoot,
			"path": target,
		})
	}
	return nil, initOut{
		Path:     target,
		Created:  created,
		Bytes:    len(body),
		Scope:    "repo",
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
	writeMCPEvent(context.Background(), store.EvtMCPSchemaInstall, "installed "+resolved, 0, map[string]string{
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

// ─── registry mutations (worktree + repo) ─────────────────────────

type registryRegisterIn struct {
	Repo   string `json:"repo,omitempty"`
	Path   string `json:"path"             jsonschema:"absolute worktree path"`
	Branch string `json:"branch,omitempty" jsonschema:"branch name; derived from .git/HEAD when omitted"`
}
type registryRegisterOut struct {
	WorktreeID int64  `json:"worktree_id"`
	RepoID     int64  `json:"repo_id"`
	Slug       string `json:"slug"`
	Branch     string `json:"branch,omitempty"`
	Path       string `json:"path"`
}

func registryRegisterTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in registryRegisterIn,
) (*mcpsdk.CallToolResult, registryRegisterOut, error) {
	if in.Path == "" {
		return nil, registryRegisterOut{}, errors.New("path is required")
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
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return nil, registryRegisterOut{}, fmt.Errorf("ensure repo: %w", err)
	}
	sl := slug.For(wt, branch)
	wtID, err := st.EnsureWorktree(ctx, repoID, wt, sl.Value, branch)
	if err != nil {
		return nil, registryRegisterOut{}, fmt.Errorf("ensure worktree: %w", err)
	}
	writeMCPEvent(context.Background(), store.EvtMCPRegistryRegister, "registered "+wt, repoID, map[string]string{
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
	Repo   string `json:"repo,omitempty"`
	Name   string `json:"name"              jsonschema:"slug, branch, or basename of the worktree"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"plan only — resolve the worktree row + return its path/id without marking it deleted"`
	Ack    bool   `json:"ack,omitempty"     jsonschema:"skip the elicitation confirmation prompt"`
}
type registryUnregisterOut struct {
	WorktreeID int64  `json:"worktree_id"`
	Path       string `json:"path"`
	DryRun     bool   `json:"dry_run,omitempty"`
	Refused    string `json:"refused,omitempty"`
}

func registryUnregisterTool(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	in registryUnregisterIn,
) (*mcpsdk.CallToolResult, registryUnregisterOut, error) {
	if in.Name == "" {
		return nil, registryUnregisterOut{}, errors.New("name is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, registryUnregisterOut{}, err
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, registryUnregisterOut{}, err
	}
	defer func() { _ = st.Close() }()
	repoID, err := lookupRepoID(ctx, st, repoRoot)
	if err != nil {
		return nil, registryUnregisterOut{}, fmt.Errorf("lookup repo %s: %w", repoRoot, err)
	}
	wtID, err := st.LookupWorktreeID(ctx, repoID, in.Name)
	if err != nil {
		return nil, registryUnregisterOut{}, err
	}
	if wtID == 0 {
		return nil, registryUnregisterOut{}, fmt.Errorf("no worktree matches %q", in.Name)
	}
	var path string
	_ = st.DB.QueryRowContext(ctx, `SELECT path FROM worktrees WHERE id = ?`, wtID).Scan(&path)
	if in.DryRun {
		return nil, registryUnregisterOut{WorktreeID: wtID, Path: path, DryRun: true}, nil
	}
	if ok, reason := confirmDestructive(
		ctx,
		req,
		false,
		in.Ack,
		fmt.Sprintf(
			"Mark worktree %q (id=%d, path=%s) deleted in the registry? On-disk path + databases will NOT be touched.",
			in.Name,
			wtID,
			path,
		),
	); !ok {
		return nil, registryUnregisterOut{WorktreeID: wtID, Path: path, Refused: reason}, nil
	}
	if err := st.MarkWorktreeDeleted(ctx, wtID); err != nil {
		return nil, registryUnregisterOut{}, err
	}
	writeMCPEvent(context.Background(), store.EvtMCPRegistryUnregister, "unregistered "+path, repoID, map[string]string{
		"path": path,
		"name": in.Name,
	})
	return nil, registryUnregisterOut{WorktreeID: wtID, Path: path}, nil
}

type repoRemoveIn struct {
	Repo   string `json:"repo,omitempty"`
	Force  bool   `json:"force,omitempty"   jsonschema:"remove even when active worktrees exist"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"plan only — count cascaded child rows (worktrees/events/snapshots/hook_runs) without removing anything"`
	Ack    bool   `json:"ack,omitempty"     jsonschema:"skip the elicitation confirmation prompt"`
}
type repoRemoveOut struct {
	RepoPath  string         `json:"repo"`
	Via       string         `json:"via,omitempty"        jsonschema:"daemon|sqlite — which path performed the delete"`
	DryRun    bool           `json:"dry_run,omitempty"`
	WouldDrop map[string]int `json:"would_drop,omitempty" jsonschema:"counts by table that would be cascaded on remove"`
	Refused   string         `json:"refused,omitempty"`
}

// repoRemoveTool drops a repo from the registry. Tool surface was
// renamed from registry_remove → repo_remove (the only registry_*
// tool that acts on a REPO rather than a worktree); the emitted
// event_type is still "registry_remove" so historical log_query
// filters keep working.
func repoRemoveTool(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	in repoRemoveIn,
) (*mcpsdk.CallToolResult, repoRemoveOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, repoRemoveOut{}, err
	}
	if in.DryRun {
		counts, err := countCascadedRows(ctx, repoRoot)
		if err != nil {
			return nil, repoRemoveOut{}, err
		}
		return nil, repoRemoveOut{RepoPath: repoRoot, DryRun: true, WouldDrop: counts}, nil
	}
	if ok, reason := confirmDestructive(ctx, req, false, in.Ack,
		"Remove repo "+repoRoot+" from the registry? Cascades to events/snapshots/hook_runs rows."); !ok {
		return nil, repoRemoveOut{RepoPath: repoRoot, Refused: reason}, nil
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
					return nil, repoRemoveOut{}, fmt.Errorf("daemon: %s", resp.Message)
				}
				writeMCPEvent(context.Background(), store.EvtMCPRegistryRemove, "removed "+repoRoot, 0, map[string]string{
					"repo": repoRoot,
					"via":  "daemon",
				})
				return nil, repoRemoveOut{RepoPath: repoRoot, Via: "daemon"}, nil
			}
		}
	}

	st, err := openStore(ctx)
	if err != nil {
		return nil, repoRemoveOut{}, err
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.LookupRepoID(ctx, repoRoot)
	if err != nil {
		return nil, repoRemoveOut{}, err
	}
	if repoID == 0 {
		return nil, repoRemoveOut{}, fmt.Errorf("repo not enrolled: %s", repoRoot)
	}
	if !in.Force {
		n, err := st.CountActiveWorktreesForRepo(ctx, repoID)
		if err != nil {
			return nil, repoRemoveOut{}, err
		}
		if n > 0 {
			return nil, repoRemoveOut{}, fmt.Errorf("repo has %d active worktree(s); pass force=true to override", n)
		}
	}
	if err := st.RemoveRepo(ctx, repoID); err != nil {
		return nil, repoRemoveOut{}, err
	}
	writeMCPEvent(context.Background(), store.EvtMCPRegistryRemove, "removed "+repoRoot, 0, map[string]string{
		"repo": repoRoot,
		"via":  "sqlite",
	})
	return nil, repoRemoveOut{RepoPath: repoRoot, Via: "sqlite"}, nil
}

type registryRepairIn struct {
	Repo string `json:"repo,omitempty"`
}

func registryRepairTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in registryRepairIn,
) (*mcpsdk.CallToolResult, wtreg.RepairResult, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, wtreg.RepairResult{}, err
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, wtreg.RepairResult{}, err
	}
	defer func() { _ = st.Close() }()
	res, err := wtreg.Repair(ctx, st, repoRoot, detectBranch)
	if err == nil && (len(res.Registered) > 0 || len(res.Unregistered) > 0) {
		writeMCPEvent(context.Background(), store.EvtMCPRegistryRepair,
			fmt.Sprintf("repaired %d registered, %d unregistered", len(res.Registered), len(res.Unregistered)),
			0, map[string]string{"repo": repoRoot})
	}
	return nil, res, err
}

// ─── snapshots_purge ──────────────────────────────────────────────

type snapshotsPurgeIn struct {
	Repo   string `json:"repo,omitempty"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"plan only — return how many snapshots WOULD be dropped (with their fingerprints + engines) without touching engines or SQLite"`
	Ack    bool   `json:"ack,omitempty"     jsonschema:"skip the elicitation confirmation prompt"`
}
type snapshotsPurgeOut struct {
	Repo          string             `json:"repo"`
	Dropped       int                `json:"dropped"`
	Errors        []string           `json:"errors,omitempty"`
	DryRun        bool               `json:"dry_run,omitempty"`
	WouldDrop     int                `json:"would_drop,omitempty"`
	WouldDropRows []snapshotsListRow `json:"would_drop_rows,omitempty"`
	Refused       string             `json:"refused,omitempty"`
}

func snapshotsPurgeTool(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	in snapshotsPurgeIn,
) (*mcpsdk.CallToolResult, snapshotsPurgeOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, snapshotsPurgeOut{}, err
	}
	if in.DryRun {
		// Reuse the same snapshots_list logic so dry_run = "exactly
		// what snapshots_list would show, framed as a purge preview."
		list, err := collectRepoSnapshots(ctx, repoRoot, 500)
		if err != nil {
			return nil, snapshotsPurgeOut{}, err
		}
		return nil, snapshotsPurgeOut{
			Repo:          repoRoot,
			DryRun:        true,
			WouldDrop:     len(list.Snapshots),
			WouldDropRows: list.Snapshots,
		}, nil
	}
	if ok, reason := confirmDestructive(ctx, req, false, in.Ack,
		"Purge ALL cached snapshots for "+repoRoot+"? Every prepare will cold-rebuild from scratch."); !ok {
		return nil, snapshotsPurgeOut{Repo: repoRoot, Refused: reason}, nil
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, snapshotsPurgeOut{}, fmt.Errorf("load config: %w", err)
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, snapshotsPurgeOut{}, err
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return nil, snapshotsPurgeOut{}, fmt.Errorf("ensure repo: %w", err)
	}
	dropped, errs := snapshot.PurgeRepo(ctx, &cfg, st, repoID)
	writeMCPEvent(context.Background(), store.EvtMCPSnapshotsPurge,
		fmt.Sprintf("purged %d snapshot(s)", dropped), repoID,
		map[string]string{"repo": repoRoot, "dropped": strconv.Itoa(dropped)})
	out := snapshotsPurgeOut{Repo: repoRoot, Dropped: dropped}
	for _, e := range errs {
		out.Errors = append(out.Errors, e.Error())
	}
	return nil, out, nil
}

// ─── logs_purge ───────────────────────────────────────────────────

type logsPurgeIn struct {
	Repo       string   `json:"repo,omitempty"`
	Worktree   string   `json:"worktree,omitempty"    jsonschema:"slug, branch, or basename"`
	OlderThan  string   `json:"older_than,omitempty"  jsonschema:"duration (24h, 7d) or RFC3339 cutoff — events older than this are purged"`
	Levels     []string `json:"levels,omitempty"`
	EventTypes []string `json:"event_types,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"     jsonschema:"plan only — report how many rows the filter currently matches without deleting"`
	Ack        bool     `json:"ack,omitempty"         jsonschema:"skip the elicitation confirmation prompt"`
}
type logsPurgeOut struct {
	RowsRemoved int64  `json:"rows_removed"`
	DryRun      bool   `json:"dry_run,omitempty"`
	WouldRemove int64  `json:"would_remove,omitempty"`
	Refused     string `json:"refused,omitempty"`
}

func logsPurgeTool(ctx context.Context, req *mcpsdk.CallToolRequest, in logsPurgeIn) (*mcpsdk.CallToolResult, logsPurgeOut, error) {
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
	defer func() { _ = st.Close() }()
	if err := applyRepoWorktreeFilter(ctx, st, &f, in.Repo, in.Worktree); err != nil {
		return nil, logsPurgeOut{}, err
	}
	if in.DryRun {
		n, err := st.CountEvents(ctx, f)
		if err != nil {
			return nil, logsPurgeOut{}, err
		}
		return nil, logsPurgeOut{DryRun: true, WouldRemove: n}, nil
	}
	// Quote the matched count in the confirmation so the user sees
	// the blast radius before approving.
	matched, _ := st.CountEvents(ctx, f)
	if ok, reason := confirmDestructive(ctx, req, false, in.Ack,
		fmt.Sprintf("Purge %d event-log row(s) matching the filter?", matched)); !ok {
		return nil, logsPurgeOut{Refused: reason}, nil
	}
	n, err := st.PurgeEvents(ctx, f)
	if err != nil {
		return nil, logsPurgeOut{}, err
	}
	writeMCPEvent(context.Background(), store.EvtMCPLogsPurge,
		fmt.Sprintf("purged %d event row(s)", n), f.RepoID,
		map[string]string{"rows_removed": strconv.FormatInt(n, 10)})
	return nil, logsPurgeOut{RowsRemoved: n}, nil
}

// parsePurgeCutoff accepts "24h" / "7d" / RFC3339 timestamps. For
// durations the cutoff is interpreted as "older than now-d"; for an
// absolute timestamp it's the timestamp itself.
func parsePurgeCutoff(s string) (time.Time, error) {
	if before, ok := strings.CutSuffix(s, "d"); ok {
		days := before
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
	Branch  string `json:"branch"             jsonschema:"branch name for the new worktree"`
	From    string `json:"from,omitempty"     jsonschema:"base branch"`
	Path    string `json:"path,omitempty"     jsonschema:"explicit worktree path"`
	Repo    string `json:"repo,omitempty"`
	NoFetch bool   `json:"no_fetch,omitempty"`
}

func worktreeCreateTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in worktreeCreateIn,
) (*mcpsdk.CallToolResult, wt.CreateResult, error) {
	if in.Branch == "" {
		return nil, wt.CreateResult{}, errors.New("branch is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, wt.CreateResult{}, fmt.Errorf("resolve repo: %w", err)
	}
	task := rpc.Task{
		Type:         rpc.TaskWorktreeCreate,
		RepoPath:     repoRoot,
		Params:       map[string]string{"branch": in.Branch},
		InheritedEnv: inheritedEnv(),
	}
	if in.From != "" {
		task.Params["from"] = in.From
	}
	if in.Path != "" {
		task.Params["path"] = in.Path
	}
	if in.NoFetch {
		task.Params["no_fetch"] = "1"
	}
	res, err := dispatchCreatePlan(ctx, task)
	return nil, res, err
}

// dispatchCreatePlan submits a worktree_create plan (result mode) to the
// daemon — the sole mutator — starting it once on connection failure,
// and parses the returned CreateResult.
func dispatchCreatePlan(ctx context.Context, task rpc.Task) (wt.CreateResult, error) {
	resp := submitMCPPlan(ctx, rpc.Plan(true, rpc.One(task)))
	if err := mcpPlanErr(resp); err != nil {
		return wt.CreateResult{}, fmt.Errorf("create: %w", err)
	}
	if len(resp.TaskResults) == 0 {
		return wt.CreateResult{}, errors.New("no task result")
	}
	var res wt.CreateResult
	if err := json.Unmarshal([]byte(resp.TaskResults[0].PayloadJSON), &res); err != nil {
		return wt.CreateResult{}, err
	}
	return res, nil
}

type worktreeDeleteIn struct {
	Name   string `json:"name"              jsonschema:"slug, branch, or basename of the worktree to delete"`
	Repo   string `json:"repo,omitempty"`
	Force  bool   `json:"force,omitempty"`
	DryRun bool   `json:"dry_run,omitempty" jsonschema:"plan only — return what would be torn down (path, DBs by engine + rendered name) without dispatching teardown"`
	Ack    bool   `json:"ack,omitempty"     jsonschema:"skip the elicitation confirmation prompt (for non-interactive agents that have already gotten user approval)"`
}

// worktreeDeleteResult is the structured response. When DryRun=true the
// `dry_run`/`would_drop` fields are populated and the wt.* fields stay
// zero (no teardown ran). Refused is set when elicitation returned
// decline/cancel.
//
// wt.DeleteResult fields are promoted explicitly rather than embedded
// so a future field add on wt.DeleteResult cannot silently collide
// with `dry_run` / `refused` / `would_*`.
type worktreeDeleteResult struct {
	WtPath          string              `json:"wt_path,omitempty"`
	Status          wt.DeleteStatus     `json:"status,omitempty"`
	LogPath         string              `json:"log_path,omitempty"`
	DryRun          bool                `json:"dry_run,omitempty"`
	WouldDropDBs    []worktreeDropEntry `json:"would_drop_dbs,omitempty"`
	WouldRemovePath string              `json:"would_remove_path,omitempty"`
	Refused         string              `json:"refused,omitempty"           jsonschema:"set when elicitation refused — value is the reason (declined|cancelled|...)"`
}

type worktreeDropEntry struct {
	Engine       string `json:"engine"`
	Name         string `json:"name"                    jsonschema:"rendered per-worktree database / prefix / index name"`
	BranchScoped bool   `json:"branch_scoped,omitempty"`
}

func worktreeDeleteTool(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	in worktreeDeleteIn,
) (*mcpsdk.CallToolResult, worktreeDeleteResult, error) {
	if in.Name == "" {
		return nil, worktreeDeleteResult{}, errors.New("name is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, worktreeDeleteResult{}, fmt.Errorf("resolve repo: %w", err)
	}
	if in.DryRun {
		plan, err := planWorktreeDelete(ctx, repoRoot, in.Name)
		if err != nil {
			return nil, worktreeDeleteResult{}, err
		}
		return nil, plan, nil
	}
	if ok, reason := confirmDestructive(ctx, req, false, in.Ack,
		fmt.Sprintf("Delete worktree %q? This drops its databases + removes the on-disk path.", in.Name)); !ok {
		return nil, worktreeDeleteResult{Refused: reason}, nil
	}
	res, err := wt.Delete(ctx, wt.DeleteRequest{
		RepoRoot: repoRoot,
		Target:   in.Name,
		Force:    in.Force,
	}, wt.NoopSink{})
	return nil, worktreeDeleteResult{
		WtPath:  res.WtPath,
		Status:  res.Status,
		LogPath: res.LogPath,
	}, err
}

// ─── daemon_control ───────────────────────────────────────────────

type daemonControlIn struct {
	Action string `json:"action"         jsonschema:"start|stop|reload|install|uninstall. reload re-reads config + restarts watchers (no process restart) — call after a config edit; repo scopes it, empty reloads all. install/uninstall manage the systemd/launchd auto-start unit."`
	Repo   string `json:"repo,omitempty" jsonschema:"reload only — scope the reload to one repo (empty = every registered repo)"`
}
type daemonControlOut struct {
	Action string `json:"action"`
	PID    int    `json:"pid,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func daemonControlTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in daemonControlIn,
) (*mcpsdk.CallToolResult, daemonControlOut, error) {
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
	case "reload":
		resp, err := rpc.Call(ctx, rpc.Request{
			Method:       rpc.MethodConfigReload,
			ConfigReload: &rpc.ConfigReloadArgs{RepoPath: in.Repo},
		})
		if err != nil {
			return nil, daemonControlOut{}, err
		}
		if resp.Kind == rpc.KindError {
			return nil, daemonControlOut{}, fmt.Errorf("daemon: %s", resp.Message)
		}
		detail := "config reloaded for all registered repos"
		if in.Repo != "" {
			detail = "config reloaded for " + in.Repo
		}
		return nil, daemonControlOut{Action: "reload", Detail: detail}, nil
	case "install":
		msg, err := daemonctl.Install(ctx)
		if err != nil {
			return nil, daemonControlOut{}, err
		}
		return nil, daemonControlOut{Action: "install", Detail: msg}, nil
	case "uninstall":
		msg, err := daemonctl.Uninstall(ctx)
		if err != nil {
			return nil, daemonControlOut{}, err
		}
		return nil, daemonControlOut{Action: "uninstall", Detail: msg}, nil
	default:
		return nil, daemonControlOut{}, fmt.Errorf("action must be start|stop|reload|install|uninstall, got %q", in.Action)
	}
}

// ─── config_set ───────────────────────────────────────────────────

type configSetIn struct {
	Repo  string `json:"repo,omitempty"`
	Path  string `json:"path"            jsonschema:"dotted path like 'daemon.gc_interval' or 'databases[0].engine'"`
	Value any    `json:"value"           jsonschema:"the value to set (scalar, array, or object). Pass null to clear."`
	Scope string `json:"scope,omitempty" jsonschema:"repo (default — .treeman.yaml) | global (~/.config/treeman/config.yaml). The top-level key is scope-checked against the target layer."`
}

type configSetOut struct {
	Path         string `json:"path"`
	Scope        string `json:"scope"`
	File         string `json:"file"`
	PreviousJSON string `json:"previous_json,omitempty"`
	NewJSON      string `json:"new_json"`
	Bytes        int    `json:"bytes"`
}

// loadConfigDoc reads p and unmarshals it into a yaml.Node. A missing or
// empty file yields a seeded empty document (a not-yet-created config can
// take its first key); other read errors surface.
func loadConfigDoc(p string) (yaml.Node, error) {
	var doc yaml.Node
	raw, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return doc, fmt.Errorf("read %s: %w", p, err)
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return doc, fmt.Errorf("parse %s: %w", p, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	return doc, nil
}

// decodePrevJSON renders the pre-patch node as JSON, or "" if absent/unencodable.
func decodePrevJSON(prev *yaml.Node) string {
	if prev == nil {
		return ""
	}
	var prevVal any
	if err := prev.Decode(&prevVal); err != nil {
		return ""
	}
	b, err := json.Marshal(prevVal)
	if err != nil {
		return ""
	}
	return string(b)
}

func configSetTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configSetIn) (*mcpsdk.CallToolResult, configSetOut, error) {
	if in.Path == "" {
		return nil, configSetOut{}, errors.New("path is required")
	}
	p, histRoot, layer, err := resolveConfigTarget(in.Scope, in.Repo)
	if err != nil {
		return nil, configSetOut{}, err
	}
	doc, err := loadConfigDoc(p)
	if err != nil {
		return nil, configSetOut{}, err
	}
	segs, err := yamlpatch.ParsePath(in.Path)
	if err != nil {
		return nil, configSetOut{}, err
	}
	if len(segs) > 0 && segs[0].Key != "" {
		if err := config.CheckKeyInLayer(segs[0].Key, layer); err != nil {
			return nil, configSetOut{}, err
		}
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
	// The global config dir may not exist on a first set; repo dirs do.
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, configSetOut{}, fmt.Errorf("create config dir: %w", err)
	}
	if err := atomicWrite(histRoot, p, body); err != nil {
		return nil, configSetOut{}, err
	}
	prevJSON := decodePrevJSON(prev)
	newJSON, err := json.Marshal(in.Value)
	if err != nil {
		return nil, configSetOut{}, fmt.Errorf("marshal value: %w", err)
	}
	writeMCPEvent(context.Background(), store.EvtMCPConfigSet, "patched "+in.Path, 0, map[string]string{
		"scope":    layer,
		"file":     p,
		"path":     in.Path,
		"new_json": string(newJSON),
	})
	return nil, configSetOut{
		Path:         in.Path,
		Scope:        layer,
		File:         p,
		PreviousJSON: prevJSON,
		NewJSON:      string(newJSON),
		Bytes:        len(body),
	}, nil
}

// ─── config_restore ───────────────────────────────────────────────

type configRestoreIn struct {
	Repo       string `json:"repo,omitempty"`
	Generation int64  `json:"generation"      jsonschema:"the generation number to restore (from config_history)"`
	Scope      string `json:"scope,omitempty" jsonschema:"repo (default — .treeman.yaml) | global (~/.config/treeman/config.yaml). Must match the scope the generation was recorded under."`
}
type configRestoreOut struct {
	Path     string `json:"path"`
	Scope    string `json:"scope"`
	Restored int64  `json:"restored"`
	Bytes    int    `json:"bytes"`
}

func configRestoreTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in configRestoreIn,
) (*mcpsdk.CallToolResult, configRestoreOut, error) {
	p, histRoot, layer, err := resolveConfigTarget(in.Scope, in.Repo)
	if err != nil {
		return nil, configRestoreOut{}, err
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, configRestoreOut{}, err
	}
	g, err := st.GetConfigGeneration(ctx, histRoot, p, in.Generation)
	_ = st.Close()
	if err != nil {
		return nil, configRestoreOut{}, fmt.Errorf("generation %d not found for %s", in.Generation, p)
	}
	if err := config.CheckBodyScope(g.Content, layer); err != nil {
		return nil, configRestoreOut{}, fmt.Errorf("generation %d cannot be restored: %w", in.Generation, err)
	}
	// The global config dir may have been deleted since the snapshot.
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, configRestoreOut{}, fmt.Errorf("create config dir: %w", err)
	}
	// atomicWrite snapshots the current content first, so the restore is
	// itself reversible.
	if err := atomicWrite(histRoot, p, g.Content); err != nil {
		return nil, configRestoreOut{}, err
	}
	writeMCPEvent(
		context.Background(),
		store.EvtMCPConfigRestore,
		fmt.Sprintf("restored generation %d", in.Generation),
		0,
		map[string]string{
			"scope":      layer,
			"file":       p,
			"generation": strconv.FormatInt(in.Generation, 10),
		},
	)
	return nil, configRestoreOut{Path: p, Scope: layer, Restored: in.Generation, Bytes: len(g.Content)}, nil
}

// ─── worktree_repair ──────────────────────────────────────────────

type worktreeRepairIn struct {
	Worktree string `json:"worktree,omitempty" jsonschema:"slug, branch, or basename; defaults to cwd's worktree"`
	Repo     string `json:"repo,omitempty"`
}

type repairAction struct {
	Action string `json:"action"           jsonschema:"registry|ports|finalize|snapshots"`
	Status string `json:"status"           jsonschema:"ok|fixed|skipped|fail"`
	Detail string `json:"detail,omitempty"`
}

type worktreeRepairOut struct {
	WorktreePath string         `json:"worktree_path"`
	WorktreeID   int64          `json:"worktree_id,omitempty"`
	Actions      []repairAction `json:"actions"`
}

// worktreeRepairTool is the recovery counterpart to registry_repair: it
// makes one worktree's registry row + ports + finalize state + snapshot
// templates consistent. Each phase is independent — a failure in one
// becomes a fail action but doesn't abort later phases.
func worktreeRepairTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in worktreeRepairIn,
) (*mcpsdk.CallToolResult, worktreeRepairOut, error) {
	wtPath, _ := resolveWorktree(in.Worktree)
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, worktreeRepairOut{}, err
	}
	out := worktreeRepairOut{WorktreePath: wtPath}

	st, err := openStore(ctx)
	if err != nil {
		return nil, out, err
	}
	defer func() { _ = st.Close() }()

	// Phase 1: registry row.
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		out.Actions = append(out.Actions, repairAction{Action: "registry", Status: "fail", Detail: err.Error()})
		return nil, out, nil
	}
	branch := detectBranch(wtPath)
	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wtPath)
	if err != nil {
		out.Actions = append(out.Actions, repairAction{Action: "registry", Status: "fail", Detail: "load config: " + err.Error()})
		return nil, out, nil
	}
	id, err := wt.ResolveIdentity(ctx, st, &cfg, repoRoot, wtPath, branch, repoID)
	if err != nil {
		out.Actions = append(out.Actions, repairAction{Action: "registry", Status: "fail", Detail: "resolve identity: " + err.Error()})
		return nil, out, nil
	}
	out.WorktreeID = id.WtID
	out.Actions = append(
		out.Actions,
		repairAction{Action: "registry", Status: "ok", Detail: fmt.Sprintf("slug=%s wt_id=%d", id.Slug.Value, id.WtID)},
	)

	// Phase 2: ports.
	out.Actions = append(out.Actions, repairPorts(ctx, st, &cfg, repoID, id.WtID))

	// Phase 3: finalize.
	out.Actions = append(out.Actions, repairFinalize(ctx, repoRoot, wtPath))

	// Phase 4: snapshot templates.
	out.Actions = append(out.Actions, repairSnapshots(ctx, st, &cfg, repoID))

	writeMCPEvent(context.Background(), store.EvtMCPWorktreeRepair, "repaired "+wtPath, repoID, map[string]string{
		"worktree_path": wtPath,
	})
	return nil, out, nil
}

// repairPorts allocates any missing port slots for the worktree.
// Returns a "skipped" action when the config declares no port slots,
// "ok" when every slot was already allocated, "fixed" when at least
// one was newly allocated.
func repairPorts(ctx context.Context, st *store.Store, cfg *config.Config, repoID, wtID int64) repairAction {
	slots := cfg.PortSlotNames()
	if len(slots) == 0 {
		return repairAction{Action: "ports", Status: "skipped", Detail: "no port slots configured"}
	}
	existing, err := st.LoadWorktreePorts(ctx, wtID)
	if err != nil {
		return repairAction{Action: "ports", Status: "fail", Detail: err.Error()}
	}
	missing := 0
	for _, s := range slots {
		if _, ok := existing[s]; !ok {
			missing++
		}
	}
	if missing == 0 {
		return repairAction{Action: "ports", Status: "ok", Detail: fmt.Sprintf("%d slot(s) already allocated", len(existing))}
	}
	// Re-allocate the whole set so the existing port → slot mapping is
	// preserved (ports.New().Allocate skips slots that already have a
	// row via the unique constraint).
	if _, err := ports.New().Allocate(ctx, st, cfg, repoID, wtID); err != nil {
		return repairAction{Action: "ports", Status: "fail", Detail: err.Error()}
	}
	return repairAction{Action: "ports", Status: "fixed", Detail: fmt.Sprintf("allocated %d missing slot(s)", missing)}
}

// repairFinalize dispatches a fresh finalize via the daemon. When the
// daemon is unreachable, returns a "skipped" — repair doesn't run
// finalize inline (a synchronous in-process cold build could block
// the MCP tool call for minutes). Caller can follow with prepare_run.
func repairFinalize(ctx context.Context, repoRoot, wtPath string) repairAction {
	queued := wt.DispatchFinalize(ctx, repoRoot, wtPath, captureEnv(), wt.NoopSink{})
	if !queued {
		return repairAction{
			Action: "finalize",
			Status: "skipped",
			Detail: "daemon unreachable — follow with prepare_run for an inline build",
		}
	}
	return repairAction{Action: "finalize", Status: "fixed", Detail: "queued via daemon"}
}

// repairSnapshots probes each snapshot row owned by repoID to confirm
// the engine-side template still exists. Orphans (template missing)
// are reported but NOT auto-dropped — the agent should confirm before
// calling snapshot_drop. Returns ok when every row is healthy.
func repairSnapshots(ctx context.Context, st *store.Store, cfg *config.Config, repoID int64) repairAction {
	rows, err := st.ListSnapshotsForRepo(ctx, repoID)
	if err != nil {
		return repairAction{Action: "snapshots", Status: "fail", Detail: err.Error()}
	}
	if len(rows) == 0 {
		return repairAction{Action: "snapshots", Status: "ok", Detail: "no snapshots cached"}
	}
	orphans := 0
	for _, r := range rows {
		exists, _, _ := probeTemplate(ctx, cfg, r.Engine, r.TemplateName)
		if !exists {
			orphans++
		}
	}
	if orphans == 0 {
		return repairAction{Action: "snapshots", Status: "ok", Detail: fmt.Sprintf("%d snapshot(s) all healthy", len(rows))}
	}
	return repairAction{
		Action: "snapshots",
		Status: "fixed",
		Detail: fmt.Sprintf("%d orphan(s) out of %d — call snapshot_drop on each, or the cache-cleanup prompt", orphans, len(rows)),
	}
}

// countCascadedRows tallies the SQLite rows that a repo_remove would
// cascade-delete. Returns counts per table so the agent can quote the
// blast radius to the user before confirming. A missing repo returns
// an empty map, not an error.
func countCascadedRows(ctx context.Context, repoRoot string) (map[string]int, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.LookupRepoID(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	if repoID == 0 {
		return map[string]int{}, nil
	}
	counts := map[string]int{}
	for tbl, q := range map[string]string{
		"worktrees": `SELECT COUNT(*) FROM worktrees WHERE repo_id = ?`,
		"events":    `SELECT COUNT(*) FROM events WHERE repo_id = ?`,
		"snapshots": `SELECT COUNT(*) FROM snapshots WHERE repo_id = ?`,
		"hook_runs": `SELECT COUNT(*) FROM hook_runs WHERE repo_id = ?`,
	} {
		var n int
		_ = st.DB.QueryRowContext(ctx, q, repoID).Scan(&n)
		counts[tbl] = n
	}
	return counts, nil
}

// planWorktreeDelete computes a dry-run plan: what path would be
// removed, and which per-engine namespaces the teardown would drop.
// Errors only on unresolvable target / repo; engine misconfiguration
// produces an empty-name entry rather than failing the plan (the
// agent should see "drop X but engine Y not configured" as data).
func planWorktreeDelete(ctx context.Context, repoRoot, target string) (worktreeDeleteResult, error) {
	out := worktreeDeleteResult{DryRun: true}
	st, err := openStore(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = st.Close() }()
	wtPath, ok := wt.LookupWorktree(ctx, repoRoot, target, wt.NoopSink{})
	if !ok {
		abs, absErr := filepath.Abs(target)
		if absErr != nil {
			return out, absErr
		}
		wtPath = abs
	}
	out.WouldRemovePath = wtPath
	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wtPath)
	if err != nil {
		return out, fmt.Errorf("load resolved config: %w", err)
	}
	branch := detectBranch(wtPath)
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	id, err := wt.ResolveIdentity(ctx, st, &cfg, repoRoot, wtPath, branch, repoID)
	if err != nil {
		return out, fmt.Errorf("resolve identity: %w", err)
	}
	tplCtx := template.FromSlug(id.Slug)
	for _, d := range cfg.Databases {
		entry := worktreeDropEntry{Engine: d.Engine, BranchScoped: d.BranchScoped}
		tmpl := d.NameTemplate
		if tmpl == "" {
			tmpl = d.KeyPrefix
		}
		if rendered, rErr := template.Render(tmpl, tplCtx); rErr == nil {
			entry.Name = rendered
		}
		out.WouldDropDBs = append(out.WouldDropDBs, entry)
	}
	return out, nil
}
