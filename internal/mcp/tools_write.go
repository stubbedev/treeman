package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/stubbedev/treeman/internal/wtreg"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// registerWriteTools binds tools that mutate state. Gated by
// Options.AllowMutations in Serve. Tools that shell out to the
// `treeman` binary (worktree_create, worktree_delete, init,
// schema_install) are further gated by AllowShellOps.
func registerWriteTools(srv *mcpsdk.Server, opts Options) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "prepare_run",
		Description: "Run the prepare pipeline for a worktree (ensure → dump → migrate → snapshot → replicate). Foreground; blocks until every engine returns an outcome.",
	}, prepareTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "hook_run",
		Description: "Run one configured hook phase synchronously. Accepts precreate|postcreate|predelete|postdelete. Returns per-group exit codes and stdout/stderr tails.",
	}, hookTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_write",
		Description: "Replace .treeman.yaml with the supplied body. The body is parsed into config.Config first; the write only happens if parsing succeeds, so invalid YAML never lands on disk. Returns the byte count written.",
	}, configWriteTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_set",
		Description: "Patch a single field of .treeman.yaml addressed by a dotted path (e.g. 'daemon.gc_interval' or 'databases[0].engine'). Preserves surrounding comments + key ordering by editing the YAML AST in place. Creates missing intermediate mapping keys; refuses to extend sequences. The result is validated by parsing into config.Config before the write lands. Returns the previous + new value as JSON.",
	}, configSetTool)

	// In-process registry mutations. No shell-out, no daemon dependency
	// — agents can reconcile drift the same way `treeman wt
	// register|unregister` would.
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "registry_register",
		Description: "Register a worktree in the SQLite registry without touching git. Provide path (absolute) and branch; slug is computed automatically when omitted. Returns the upserted worktree row id.",
	}, registryRegisterTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "registry_unregister",
		Description: "Mark a worktree deleted in SQLite without touching git. name resolves by slug, branch, or basename. Idempotent.",
	}, registryUnregisterTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "registry_repair",
		Description: "Reconcile the SQLite registry with `git worktree list` for the current (or specified) repo: register paths git knows that SQLite doesn't, and mark deleted those SQLite knows that git doesn't. Returns per-action counts.",
	}, registryRepairTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "registry_remove",
		Description: "Drop a repo from the SQLite registry. Stops any live daemon watchers attached to the repo and deletes child rows (worktrees, events, snapshots, binlog_checkpoints, hook_runs). External resources (databases, on-disk worktree dirs, dump caches) are NOT touched. Refuses by default if active worktrees still exist — pass force=true to override.",
	}, registryRemoveTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_watch",
		Description: "Toggle the per-repo opt-in for the daemon's lifecycle watcher. With opt-in ON, the daemon tails `<common-dir>/worktrees/` and fires postcreate / postdelete hooks automatically when `git worktree add`/`remove` runs outside the treeman CLI. Action must be one of: on, off, status. Both this opt-in AND the resolved config bool `worktrees.hook_lifecycle: true` must be set for the watcher to activate.",
	}, worktreeWatchTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "snapshots_purge",
		Description: "Drop every cached snapshot (template DB) belonging to the current (or specified) repo. Frees engine-side storage and forces the next prepare to rebuild from scratch. Returns counts + any per-engine errors.",
	}, snapshotsPurgeTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "logs_purge",
		Description: "Delete event-log rows. Filters are AND-combined; pass older_than=24h to drop anything older than the cutoff. All filters are optional, but at least one must be specified to prevent accidental full wipes. Returns the row count removed.",
	}, logsPurgeTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "schema_install",
		Description: "Generate the JSON Schema for .treeman.yaml and wire it into the yaml-language-server modeline. target=repo writes <repo>/schemas/treeman.schema.json (default). target=global writes the user-XDG path so multiple repos share one file. target=url skips the file write and points the modeline at the canonical upstream URL.",
	}, schemaInstallTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "init_repo",
		Description: "Scaffold .treeman.yaml under cwd (or --repo). Detects migration framework + JS package manager and emits matching databases/hooks blocks. Pass force=true to overwrite an existing file. Returns the chosen path, byte count, and detected framework names.",
	}, initRepoTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "daemon_control",
		Description: "Start or stop treemand. Action must be one of: start, stop. Prefers the installed systemd/launchd unit when present; otherwise forks the treemand binary (start) or sends the shutdown RPC (stop). In-process — no shell-out.",
	}, daemonControlTool)

	if !opts.AllowShellOps {
		return
	}

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_create",
		Description: "Create a new git worktree under .worktrees/<branch> and dispatch postcreate hooks + prepare via the daemon. Shells to `treeman wt create`. Long-running. Returns the captured stdout/stderr and exit code.",
	}, worktreeCreateTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_delete",
		Description: "Tear down a worktree: predelete hooks → DB teardown → git worktree remove. Shells to `treeman wt delete`. Returns the captured stdout/stderr and exit code.",
	}, worktreeDeleteTool)
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
	Phase    string `json:"phase" jsonschema:"precreate|postcreate|predelete|postdelete"`
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

// ─── shell-out helpers ────────────────────────────────────────────

type shellOut struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// runTreeman shells out to the treeman binary (same one currently
// executing when possible) with the given arg list. cwd controls
// the worktree the CLI sees; pass "" for the current process cwd.
func runTreeman(ctx context.Context, cwd string, args ...string) (shellOut, error) {
	bin, err := treemanBinary()
	if err != nil {
		return shellOut{}, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return shellOut{}, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return shellOut{}, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return shellOut{}, err
	}
	outBytes, _ := readAll(stdout)
	errBytes, _ := readAll(stderr)
	waitErr := cmd.Wait()
	code := 0
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if waitErr != nil {
		code = -1
	}
	return shellOut{ExitCode: code, Stdout: string(outBytes), Stderr: string(errBytes)}, nil
}

func treemanBinary() (string, error) {
	if p, err := os.Executable(); err == nil {
		return p, nil
	}
	return exec.LookPath("treeman")
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	const max = 1 << 20 // 1 MiB cap so a runaway hook can't OOM the mcp server
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) >= max {
				buf = buf[:max]
				break
			}
		}
		if err != nil {
			break
		}
	}
	return buf, nil
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

// ─── worktree_watch ───────────────────────────────────────────────

type worktreeWatchIn struct {
	Repo   string `json:"repo,omitempty"`
	Action string `json:"action" jsonschema:"on|off|status"`
}
type worktreeWatchOut struct {
	Repo          string `json:"repo"`
	OptIn         bool   `json:"opt_in"`
	ConfigEnabled bool   `json:"config_enabled"`
	Active        bool   `json:"active"`
}

func worktreeWatchTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeWatchIn) (*mcpsdk.CallToolResult, worktreeWatchOut, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "" {
		action = "status"
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, worktreeWatchOut{}, err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, worktreeWatchOut{}, fmt.Errorf("load config: %w", err)
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, worktreeWatchOut{}, err
	}
	defer st.Close()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return nil, worktreeWatchOut{}, fmt.Errorf("ensure repo: %w", err)
	}

	configEnabled := cfg.Worktrees.HookLifecycle != nil && *cfg.Worktrees.HookLifecycle

	switch action {
	case "on", "enable":
		if err := st.SetRepoWatchLifecycle(ctx, repoID, true); err != nil {
			return nil, worktreeWatchOut{}, err
		}
		writeMCPEvent(context.Background(), "worktree_watch", "opted in "+repoRoot, repoID, map[string]string{
			"repo":   repoRoot,
			"action": "on",
		})
	case "off", "disable":
		if err := st.SetRepoWatchLifecycle(ctx, repoID, false); err != nil {
			return nil, worktreeWatchOut{}, err
		}
		writeMCPEvent(context.Background(), "worktree_watch", "opted out "+repoRoot, repoID, map[string]string{
			"repo":   repoRoot,
			"action": "off",
		})
	case "status":
		// read-only
	default:
		return nil, worktreeWatchOut{}, fmt.Errorf("unknown action %q (want on|off|status)", in.Action)
	}

	on, _ := st.GetRepoWatchLifecycle(ctx, repoID)
	return nil, worktreeWatchOut{
		Repo:          repoRoot,
		OptIn:         on,
		ConfigEnabled: configEnabled,
		Active:        on && configEnabled,
	}, nil
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

func worktreeCreateTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeCreateIn) (*mcpsdk.CallToolResult, shellOut, error) {
	if in.Branch == "" {
		return nil, shellOut{}, fmt.Errorf("branch is required")
	}
	args := []string{"wt", "create", in.Branch}
	if in.From != "" {
		args = append(args, "--from", in.From)
	}
	if in.Path != "" {
		args = append(args, "--path", in.Path)
	}
	if in.NoFetch {
		args = append(args, "--no-fetch")
	}
	cwd, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, shellOut{}, fmt.Errorf("resolve repo: %w", err)
	}
	out, err := runTreeman(ctx, cwd, args...)
	return nil, out, err
}

type worktreeDeleteIn struct {
	Name  string `json:"name" jsonschema:"slug, branch, or basename of the worktree to delete"`
	Repo  string `json:"repo,omitempty"`
	Force bool   `json:"force,omitempty"`
}

func worktreeDeleteTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeDeleteIn) (*mcpsdk.CallToolResult, shellOut, error) {
	if in.Name == "" {
		return nil, shellOut{}, fmt.Errorf("name is required")
	}
	args := []string{"wt", "delete", in.Name}
	if in.Force {
		args = append(args, "--force")
	}
	cwd, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, shellOut{}, fmt.Errorf("resolve repo: %w", err)
	}
	out, err := runTreeman(ctx, cwd, args...)
	return nil, out, err
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
