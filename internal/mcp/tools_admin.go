package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/notify"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// registerAdminGapTools binds the lifecycle/admin tools that close the
// last CLI↔MCP gaps: async finalize, main-worktree enrollment, the
// cross-repo status rollup, and the desktop-notification self-test.
func registerAdminGapTools(srv *mcpsdk.Server) {
	addTool(srv, &mcpsdk.Tool{
		Name:        "worktree_finalize",
		Description: "Re-run a worktree's setup hooks + prepare via the daemon (async — returns once queued, follow with logs_subscribe/logs_wait). The recovery/repeat counterpart to worktree_create's tail: use after a failed prepare, a config change, or to (re)materialize DBs without recreating the git worktree. Requires treemand running (daemon_control action=start).",
		Annotations: writeAnno("Finalize worktree", false, false, true),
	}, worktreeFinalizeTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "main_worktree",
		Description: "Manage main-worktree enrollment — opt the repo ROOT into the per-branch DB lifecycle. action=enable (patch main_worktree.enabled=true, reload daemon, queue setup+prepare) | disable (set false, reload; purge=true also drops every main_<branch> DB) | status (enabled flag + enrolled row). Edits .treeman.yaml's main_worktree block.",
		Annotations: writeAnno("Main-worktree enrollment", true, false, true),
	}, mainWorktreeTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "status_overview",
		Description: "Cross-repo fleet rollup: every registered worktree across every repo, bucketed stable/up(preparing)/down(teardown)/failed, derived from the latest lifecycle event per worktree. The fleet-wide answer to \"what's the state of everything?\" — worktree_list is per-repo and has no health bucket.",
		Annotations: readOnlyAnno("Status overview", false),
	}, statusOverviewTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "notify_test",
		Description: "Send a test desktop notification through the configured (or auto-detected) backend to verify notify-send (Linux) / osascript (macOS) works before enabling notifications. Ignores notifications.enabled — it tests the transport. backend overrides the global config's notifications.backend.",
		Annotations: writeAnno("Test notification", false, true, true),
	}, notifyTestTool)
}

// inheritedEnv snapshots os.Environ() into a map so a daemon-dispatched
// finalize sees the agent's PATH / shims, not the daemon's bare env.
func inheritedEnv() map[string]string {
	env := os.Environ()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// finalizePlan wraps the worktree-finalize tail as a fire-and-forget
// run_plan request — the generic channel the daemon uses for every
// offloaded mutation.
func finalizePlan(repoRoot, wtPath string) rpc.Request {
	return rpc.Plan(false, rpc.One(rpc.Task{
		Type:         rpc.TaskWorktreeFinalize,
		RepoPath:     repoRoot,
		WorktreePath: wtPath,
		InheritedEnv: inheritedEnv(),
	}))
}

// submitMCPPlan dispatches a plan to the daemon, falling back to running
// it in-process when no daemon is reachable — mirroring the CLI so MCP
// tools work daemon-less too.
func submitMCPPlan(ctx context.Context, req rpc.Request) rpc.Response {
	if resp, err := rpc.Call(ctx, req); err == nil {
		return resp
	}
	resp, err := daemon.RunPlanInProcess(ctx, *req.RunPlan)
	if err != nil {
		return rpc.Response{Kind: rpc.KindError, Message: err.Error()}
	}
	return resp
}

// mcpPlanErr collapses a plan response to an error — KindError, or the
// first failed task — or nil on success.
func mcpPlanErr(resp rpc.Response) error {
	if resp.Kind == rpc.KindError {
		return errors.New(resp.Message)
	}
	for _, r := range resp.TaskResults {
		if !r.OK {
			return errors.New(r.Message)
		}
	}
	return nil
}

// ─── worktree_finalize ─────────────────────────────────────────────

type worktreeFinalizeIn struct {
	Worktree string `json:"worktree,omitempty" jsonschema:"slug, branch, basename, or path; defaults to cwd's worktree"`
	Repo     string `json:"repo,omitempty"`
}
type worktreeFinalizeOut struct {
	WorktreePath string `json:"worktree_path"`
	Queued       bool   `json:"queued"`
	Detail       string `json:"detail"`
}

func worktreeFinalizeTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in worktreeFinalizeIn,
) (*mcpsdk.CallToolResult, worktreeFinalizeOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, worktreeFinalizeOut{}, err
	}
	wtPath, _ := resolveWorktree(in.Worktree)
	if wtPath == "" {
		wtPath = repoRoot
	}
	if err := mcpPlanErr(submitMCPPlan(ctx, finalizePlan(repoRoot, wtPath))); err != nil {
		return nil, worktreeFinalizeOut{}, fmt.Errorf("finalize: %w", err)
	}
	writeMCPEvent(context.Background(), store.EvtMCPWorktreeFinalize, "queued finalize for "+wtPath, 0, map[string]string{
		"repo":     repoRoot,
		"worktree": wtPath,
	})
	return nil, worktreeFinalizeOut{
		WorktreePath: wtPath,
		Queued:       true,
		Detail:       "setup hooks + prepare queued on the daemon — follow with logs_subscribe or logs_wait",
	}, nil
}

// ─── main_worktree ─────────────────────────────────────────────────

type mainWorktreeIn struct {
	Action string `json:"action"          jsonschema:"enable|disable|status"`
	Repo   string `json:"repo,omitempty"`
	Purge  bool   `json:"purge,omitempty" jsonschema:"disable only — after disabling, drop every per-branch DB the main_<branch> slug owns (engine resources only; the row stays soft-deleted)"`
}
type mainWorktreeOut struct {
	Action        string `json:"action"`
	Repo          string `json:"repo"`
	Enabled       bool   `json:"enabled"`
	CurrentBranch string `json:"current_branch,omitempty"`
	EnrolledSlug  string `json:"enrolled_slug,omitempty"`
	EnrolledRowID int64  `json:"enrolled_row_id,omitempty"`
	Purged        int    `json:"purged_branches,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

func mainWorktreeTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in mainWorktreeIn) (*mcpsdk.CallToolResult, mainWorktreeOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, mainWorktreeOut{}, err
	}
	switch strings.ToLower(in.Action) {
	case "status":
		return mainWorktreeStatus(ctx, repoRoot)
	case "enable":
		return mainWorktreeEnable(ctx, repoRoot)
	case "disable":
		return mainWorktreeDisable(ctx, repoRoot, in.Purge)
	default:
		return nil, mainWorktreeOut{}, fmt.Errorf("action must be enable|disable|status, got %q", in.Action)
	}
}

func mainWorktreeStatus(ctx context.Context, repoRoot string) (*mcpsdk.CallToolResult, mainWorktreeOut, error) {
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, mainWorktreeOut{}, fmt.Errorf("load config: %w", err)
	}
	out := mainWorktreeOut{Action: "status", Repo: repoRoot, Enabled: cfg.MainWorktree.Enabled, CurrentBranch: detectBranch(repoRoot)}
	st, err := openStore(ctx)
	if err == nil {
		defer func() { _ = st.Close() }()
		if repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot)); err == nil && repoID != 0 {
			if row, err := st.LookupMainWorktree(ctx, repoID); err == nil {
				out.EnrolledSlug = row.Slug
				out.EnrolledRowID = row.ID
			}
		}
	}
	return nil, out, nil
}

func mainWorktreeEnable(ctx context.Context, repoRoot string) (*mcpsdk.CallToolResult, mainWorktreeOut, error) {
	if err := patchMainWorktreeEnabled(repoRoot, true); err != nil {
		return nil, mainWorktreeOut{}, err
	}
	out := mainWorktreeOut{Action: "enable", Repo: repoRoot, Enabled: true, CurrentBranch: detectBranch(repoRoot)}
	if err := requestConfigReload(ctx, repoRoot); err != nil {
		out.Detail = "config written but daemon reload failed (" + err.Error() + ") — restart treemand to apply"
		return nil, out, nil //nolint:nilerr // failure reported via out.Detail, not as a transport error
	}
	// Reload is synchronous; the main row exists by the time it returns,
	// so finalize routes through the main-wt path and fires on-create-*.
	if err := mcpPlanErr(submitMCPPlan(ctx, finalizePlan(repoRoot, repoRoot))); err != nil {
		out.Detail = "enabled + reloaded; finalize failed (" + err.Error() + ") — run worktree_finalize at the repo root to retry"
		return nil, out, nil //nolint:nilerr // failure reported via out.Detail, not as a transport error
	}
	out.Detail = "enabled — setup hooks + prepare queued (follow with logs_subscribe)"
	writeMCPEvent(context.Background(), store.EvtMCPMainWorktree, "enabled "+repoRoot, 0, map[string]string{"repo": repoRoot})
	return nil, out, nil
}

func mainWorktreeDisable(ctx context.Context, repoRoot string, purge bool) (*mcpsdk.CallToolResult, mainWorktreeOut, error) {
	// Snapshot config BEFORE patching — purge needs the connections +
	// templates we're about to disable.
	var cfgForPurge *config.Config
	if purge {
		cfg, err := resolve.LoadResolved(repoRoot)
		if err != nil {
			return nil, mainWorktreeOut{}, fmt.Errorf("load config for purge: %w", err)
		}
		cfgForPurge = &cfg
	}
	if err := patchMainWorktreeEnabled(repoRoot, false); err != nil {
		return nil, mainWorktreeOut{}, err
	}
	out := mainWorktreeOut{Action: "disable", Repo: repoRoot, Enabled: false, CurrentBranch: detectBranch(repoRoot)}
	if err := requestConfigReload(ctx, repoRoot); err != nil {
		out.Detail = "config written but daemon reload failed (" + err.Error() + ") — restart treemand to apply"
		return nil, out, nil //nolint:nilerr // failure reported via out.Detail, not as a transport error
	}
	if cfgForPurge != nil {
		n, err := purgeMainDatabases(ctx, repoRoot, cfgForPurge)
		if err != nil {
			out.Detail = "disabled; purge failed: " + err.Error()
			return nil, out, nil //nolint:nilerr // failure reported via out.Detail, not as a transport error
		}
		out.Purged = n
	}
	out.Detail = "main worktree disabled"
	writeMCPEvent(context.Background(), store.EvtMCPMainWorktree, "disabled "+repoRoot, 0, map[string]string{"repo": repoRoot})
	return nil, out, nil
}

// patchMainWorktreeEnabled surgically sets main_worktree.enabled in the
// repo's .treeman.yaml (creating a minimal file if none exists),
// preserving comments + snapshotting prior content for config_restore.
func patchMainWorktreeEnabled(repoRoot string, enabled bool) error {
	target := filepath.Join(repoRoot, ".treeman.yaml")
	raw, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", target, err)
	}
	var doc yaml.Node
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", target, err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	segs, err := yamlpatch.ParsePath("main_worktree.enabled")
	if err != nil {
		return err
	}
	node, err := yamlpatch.ValueToNode(enabled)
	if err != nil {
		return fmt.Errorf("encode value: %w", err)
	}
	if _, err := yamlpatch.Set(&doc, segs, node); err != nil {
		return err
	}
	body, err := yamlpatch.Marshal(&doc)
	if err != nil {
		return err
	}
	return atomicWrite(repoRoot, target, body)
}

// requestConfigReload tells the daemon to re-evaluate a repo's config so
// the new main_worktree block takes effect without a restart.
func requestConfigReload(ctx context.Context, repoRoot string) error {
	resp, err := rpc.Call(ctx, rpc.Request{
		Method:       rpc.MethodConfigReload,
		ConfigReload: &rpc.ConfigReloadArgs{RepoPath: repoRoot},
	})
	if err != nil {
		return err
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	return nil
}

// purgeMainDatabases drops the per-branch DBs the main_<branch> slug owns
// across every local branch (+ HEAD). Best-effort per branch. Returns the
// count purged.
func purgeMainDatabases(ctx context.Context, repoRoot string, cfg *config.Config) (int, error) {
	st, err := openStore(ctx)
	if err != nil {
		return 0, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return 0, fmt.Errorf("ensure repo: %w", err)
	}
	config.ApplyMainWorktreeOverlay(cfg)

	branches := localBranches(ctx, repoRoot)
	if cur := detectBranch(repoRoot); cur != "" {
		if !slices.Contains(branches, cur) {
			branches = append(branches, cur)
		}
	}
	var purged int
	for _, branch := range branches {
		sl := slug.ForMain(repoRoot, branch)
		if err := prepare.TeardownDatabases(ctx, cfg, sl.Value, repoID, 0, st); err != nil {
			continue
		}
		purged++
	}
	return purged, nil
}

func localBranches(ctx context.Context, repoRoot string) []string {
	out, err := gitcmd.String(ctx, repoRoot, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil
	}
	var branches []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}

// ─── status_overview ───────────────────────────────────────────────

type statusWorktree struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Slug   string `json:"slug"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
	IsMain bool   `json:"is_main,omitempty"`
	Path   string `json:"path"`
}
type statusOverviewOut struct {
	Total     int              `json:"total"`
	Stable    int              `json:"stable"`
	Up        int              `json:"up"`
	Down      int              `json:"down"`
	Failed    int              `json:"failed"`
	Worktrees []statusWorktree `json:"worktrees"`
}

func statusOverviewTool(ctx context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, statusOverviewOut, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, statusOverviewOut{}, err
	}
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT COALESCE(w.slug,''), COALESCE(w.branch,'-'), w.path, w.is_main, r.path, w.id
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE w.deleted_at IS NULL
		ORDER BY r.path, w.is_main DESC, w.branch`)
	if err != nil {
		return nil, statusOverviewOut{}, err
	}
	defer func() { _ = rows.Close() }()
	out := statusOverviewOut{}
	for rows.Next() {
		var (
			sl, branch, wpath, rpath string
			isMain                   bool
			wid                      int64
		)
		if err := rows.Scan(&sl, &branch, &wpath, &isMain, &rpath, &wid); err != nil {
			return nil, statusOverviewOut{}, err
		}
		state, bucket := deriveStatusBucket(ctx, st, wid)
		out.Total++
		switch bucket {
		case "stable":
			out.Stable++
		case "up":
			out.Up++
		case "down":
			out.Down++
		case "failed":
			out.Failed++
		}
		out.Worktrees = append(out.Worktrees, statusWorktree{
			Repo: filepath.Base(rpath), Branch: branch, Slug: sl,
			State: state, Bucket: bucket, IsMain: isMain, Path: wpath,
		})
	}
	return nil, out, rows.Err()
}

// deriveStatusBucket maps a worktree's latest lifecycle event to a
// (state, bucket) pair. Mirrors the CLI status widget's logic.
func deriveStatusBucket(ctx context.Context, st *store.Store, wtID int64) (state, bucket string) {
	rows, _ := st.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{
			store.EvtWorktreeCreateStart, store.EvtWorktreeCreateEnd, store.EvtWorktreeCreateError,
			store.EvtWorktreeDeleteStart, store.EvtWorktreeDeleteEnd,
			store.EvtWorktreeReapStart, store.EvtWorktreeReapEnd,
		},
		Limit: 1,
	})
	if len(rows) == 0 {
		return "ready", "stable"
	}
	switch last := rows[0]; last.EventType {
	case store.EvtWorktreeCreateStart:
		return "preparing", "up"
	case store.EvtWorktreeCreateError:
		if last.Level == "error" {
			return "error", "failed"
		}
		return "ready", "stable"
	case store.EvtWorktreeDeleteStart, store.EvtWorktreeReapStart:
		return "teardown", "down"
	default:
		return "ready", "stable"
	}
}

// ─── notify_test ───────────────────────────────────────────────────

type notifyTestIn struct {
	Backend string `json:"backend,omitempty" jsonschema:"auto | notify-send | osascript | none (default: global notifications.backend, else auto)"`
}
type notifyTestOut struct {
	Backend string `json:"backend"`
	Sent    bool   `json:"sent"`
}

func notifyTestTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in notifyTestIn) (*mcpsdk.CallToolResult, notifyTestOut, error) {
	backend := in.Backend
	if backend == "" {
		if gcfg, err := config.LoadGlobal(); err == nil {
			backend = gcfg.Notifications.Backend
		}
	}
	if backend == "none" {
		return nil, notifyTestOut{
				Backend: "none",
			}, errors.New(
				"backend is \"none\" — it mutes all notifications; pass backend=auto|notify-send|osascript to probe a real sender",
			)
	}
	sender := notify.NewSender(backend)
	if !sender.Available() {
		return nil, notifyTestOut{
				Backend: backend,
			}, fmt.Errorf(
				"notification backend %q is not available on this host — install notify-send (Linux) or run on macOS",
				backend,
			)
	}
	if err := sender.Send(ctx, notify.Notification{
		Title:   "treeman: test",
		Body:    "Desktop notifications are working.",
		Urgency: notify.UrgencyNormal,
	}); err != nil {
		return nil, notifyTestOut{Backend: backend}, fmt.Errorf("send test notification: %w", err)
	}
	return nil, notifyTestOut{Backend: backend, Sent: true}, nil
}
