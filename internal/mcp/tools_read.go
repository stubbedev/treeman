package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/schema"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/wtreg"
)

// registerReadTools binds every read-only tool to the server.
// Reads never mutate state (no shell commands, no SQLite writes,
// no file modifications) so they're always available.
func registerReadTools(srv *mcpsdk.Server) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "doctor",
		Description: "Run treeman health checks: daemon reachability, .treeman.yaml load, JSON schema install, migration framework detection, and registry/git worktree drift. Returns one result per check.",
	}, doctorTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_get",
		Description: "Load and return the resolved .treeman.yaml for the current (or specified) repo. Set resolved=true to include resolved connection strings for every configured engine.",
	}, configGetTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_validate",
		Description: "Try to load .treeman.yaml and report success or the first parse/validation error.",
	}, configValidateTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_schema",
		Description: "Return the JSON Schema for .treeman.yaml (generated via reflection from the config.Config type). Use this to drive autocomplete or to validate a proposed config diff before writing it.",
	}, configSchemaTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_list",
		Description: "List active worktrees registered in the SQLite store. Optionally filter to a single repo by path.",
	}, worktreeListTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_show",
		Description: "Return detail for one worktree: slug, branch, path, created-at timestamp, and the most recent finalize event.",
	}, worktreeShowTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "logs_query",
		Description: "Query the SQLite event log. All filters are optional and AND-combined. Levels, event_types, and phases are slice matches; since accepts duration (10m, 2h) or RFC3339; payload_like is a substring against payload_json.",
	}, logsQueryTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "logs_hooks",
		Description: "Return the most recent hook_run rows for a worktree (resolved by slug, branch, or basename).",
	}, logsHooksTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "fw_detect",
		Description: "Detect migration and test frameworks for the current (or specified) repo. Returns the same data as `treeman fw detect --json`.",
	}, fwDetectTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "slug_compute",
		Description: "Compute the slug treeman would derive for a worktree path. Useful before creating a worktree to know which Redis db / DB name it'll use.",
	}, slugComputeTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "daemon_status",
		Description: "Ask the running treemand for its version, PID, and watcher count. Returns a structured object with status=running|not-running.",
	}, daemonStatusTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "snapshots_list",
		Description: "List cached snapshots (template DBs) for the current (or specified) repo. Read-only complement to snapshots_purge so agents can see what would be wiped before purging.",
	}, snapshotsListTool)
}

// ─── doctor ───────────────────────────────────────────────────────

type doctorIn struct{}
type doctorOut struct {
	Results []doctorResult `json:"results"`
}
type doctorResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok|warn|fail|skip
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

func doctorTool(ctx context.Context, _ *mcpsdk.CallToolRequest, _ doctorIn) (*mcpsdk.CallToolResult, doctorOut, error) {
	return nil, doctorOut{Results: runDoctorChecks(ctx)}, nil
}

func runDoctorChecks(ctx context.Context) []doctorResult {
	repoRoot, _ := resolveRepo("")
	out := []doctorResult{checkDaemon(ctx)}
	if repoRoot == "" {
		return append(out, doctorResult{Name: "repo", Status: "skip", Detail: "not inside a git repo — repo-scoped checks skipped"})
	}
	out = append(out,
		checkConfig(repoRoot),
		checkSchema(repoRoot),
		checkFrameworks(repoRoot),
		checkRegistry(ctx, repoRoot),
	)
	return out
}

func checkDaemon(ctx context.Context) doctorResult {
	resp, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodStatus})
	if err != nil {
		return doctorResult{Name: "daemon", Status: "warn", Detail: "not reachable", Hint: "treeman daemon start"}
	}
	if resp.Kind == rpc.KindError {
		return doctorResult{Name: "daemon", Status: "fail", Detail: resp.Message}
	}
	return doctorResult{Name: "daemon", Status: "ok", Detail: fmt.Sprintf("treemand %s pid=%d watchers=%d", resp.DaemonVersion, resp.Pid, resp.WatcherCount)}
}

func checkConfig(repoRoot string) doctorResult {
	p := filepath.Join(repoRoot, ".treeman.yaml")
	if _, err := os.Stat(p); err != nil {
		return doctorResult{Name: "config", Status: "warn", Detail: ".treeman.yaml not found in " + repoRoot, Hint: "treeman init"}
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return doctorResult{Name: "config", Status: "fail", Detail: err.Error()}
	}
	return doctorResult{Name: "config", Status: "ok", Detail: fmt.Sprintf("loaded %s (%d databases)", p, len(cfg.Databases))}
}

func checkSchema(repoRoot string) doctorResult {
	ref := schema.ReadModeline(repoRoot)
	if ref != "" {
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			return doctorResult{Name: "schema", Status: "ok", Detail: "modeline → " + ref}
		}
		p := ref
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoRoot, p)
		}
		if _, err := os.Stat(p); err == nil {
			return doctorResult{Name: "schema", Status: "ok", Detail: "modeline → " + p}
		}
		return doctorResult{Name: "schema", Status: "warn", Detail: "modeline points to missing file: " + p, Hint: "schema_install"}
	}
	repoPath := filepath.Join(repoRoot, "schemas", "treeman.schema.json")
	if _, err := os.Stat(repoPath); err == nil {
		return doctorResult{Name: "schema", Status: "warn", Detail: repoPath + " (no modeline)", Hint: "schema_install"}
	}
	if gp, err := schema.GlobalPath(); err == nil {
		if _, err := os.Stat(gp); err == nil {
			return doctorResult{Name: "schema", Status: "warn", Detail: gp + " (no modeline)", Hint: "schema_install target=global"}
		}
	}
	return doctorResult{Name: "schema", Status: "warn", Detail: "no schema installed", Hint: "schema_install"}
}

func checkFrameworks(repoRoot string) doctorResult {
	detected := framework.DefaultRegistry().DetectAll(repoRoot)
	if len(detected) == 0 {
		return doctorResult{Name: "framework", Status: "warn", Detail: "no migration framework detected"}
	}
	names := make([]string, 0, len(detected))
	for _, s := range detected {
		names = append(names, s.Name)
	}
	return doctorResult{Name: "framework", Status: "ok", Detail: strings.Join(names, ", ")}
}

func checkRegistry(ctx context.Context, repoRoot string) doctorResult {
	gitPaths, err := gitWorktreePaths(ctx, repoRoot)
	if err != nil {
		return doctorResult{Name: "registry", Status: "fail", Detail: err.Error()}
	}
	st, err := openStore(ctx)
	if err != nil {
		return doctorResult{Name: "registry", Status: "fail", Detail: err.Error()}
	}
	defer st.Close()
	rows, err := st.DB.QueryContext(ctx, `SELECT w.path FROM worktrees w
		JOIN repos r ON r.id = w.repo_id WHERE r.path = ? AND w.deleted_at IS NULL`, repoRoot)
	if err != nil {
		return doctorResult{Name: "registry", Status: "fail", Detail: err.Error()}
	}
	defer rows.Close()
	dbPaths := map[string]bool{}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			dbPaths[p] = true
		}
	}
	gitSet := map[string]bool{}
	var onlyInGit, onlyInDB []string
	for _, p := range gitPaths {
		gitSet[p] = true
		if !dbPaths[p] {
			onlyInGit = append(onlyInGit, p)
		}
	}
	for p := range dbPaths {
		if !gitSet[p] {
			onlyInDB = append(onlyInDB, p)
		}
	}
	if len(onlyInGit) == 0 && len(onlyInDB) == 0 {
		return doctorResult{Name: "registry", Status: "ok", Detail: fmt.Sprintf("%d worktree(s) synced with git", len(dbPaths))}
	}
	parts := []string{}
	if len(onlyInGit) > 0 {
		parts = append(parts, fmt.Sprintf("%d in git but not registered: %s", len(onlyInGit), strings.Join(onlyInGit, ", ")))
	}
	if len(onlyInDB) > 0 {
		parts = append(parts, fmt.Sprintf("%d registered but missing from git: %s", len(onlyInDB), strings.Join(onlyInDB, ", ")))
	}
	return doctorResult{Name: "registry", Status: "warn", Detail: strings.Join(parts, "; "), Hint: "treeman wt register|unregister"}
}

func gitWorktreePaths(ctx context.Context, repoRoot string) ([]string, error) {
	return wtreg.GitWorktreePaths(ctx, repoRoot)
}

// ─── config_get / validate / schema ───────────────────────────────

type configGetIn struct {
	Repo     string `json:"repo,omitempty" jsonschema:"repo root override (defaults to cwd)"`
	Resolved bool   `json:"resolved,omitempty" jsonschema:"include resolved connection strings"`
}

// configGetTool returns the loaded config as a map so the SDK's
// schema inference doesn't walk into config.Config (which uses
// invopop-style jsonschema tags incompatible with jsonschema-go).
//
// Resolved connection strings carry embedded passwords. Before
// returning, the payload is round-tripped through redactSecrets so
// `mysql://user:pw@host` style userinfo and `password: "..."` key/
// value pairs are scrubbed. LLM clients see structure + which
// secrets exist, never the literal values.
func configGetTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configGetIn) (*mcpsdk.CallToolResult, map[string]any, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{"repo": repoRoot, "config": cfg}
	if in.Resolved {
		out["resolved"] = resolve.Resolve(&cfg, repoRoot)
	}
	return nil, redactMap(out), nil
}

// redactMap round-trips `m` through JSON + redactSecrets so any
// embedded password / token / URL-userinfo strings are scrubbed
// regardless of how deeply nested they sit. Cheap: configs are
// small and this only runs on tool invocation.
func redactMap(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		return m
	}
	clean := redactSecrets(string(b))
	var out map[string]any
	if err := json.Unmarshal([]byte(clean), &out); err != nil {
		return m
	}
	return out
}

type configValidateIn struct {
	Repo string `json:"repo,omitempty"`
}
type configValidateOut struct {
	OK        bool   `json:"ok"`
	Repo      string `json:"repo,omitempty"`
	Databases int    `json:"databases,omitempty"`
	Error     string `json:"error,omitempty"`
}

func configValidateTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configValidateIn) (*mcpsdk.CallToolResult, configValidateOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, configValidateOut{OK: false, Error: err.Error()}, nil
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, configValidateOut{OK: false, Repo: repoRoot, Error: err.Error()}, nil
	}
	return nil, configValidateOut{OK: true, Repo: repoRoot, Databases: len(cfg.Databases)}, nil
}

type configSchemaIn struct{}

// configSchemaTool returns the JSON Schema as a map[string]any so
// the SDK serialises it as a nested object instead of the byte-array
// shape that json.RawMessage produces by default.
func configSchemaTool(_ context.Context, _ *mcpsdk.CallToolRequest, _ configSchemaIn) (*mcpsdk.CallToolResult, map[string]any, error) {
	b, err := schema.Render()
	if err != nil {
		return nil, nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"schema": out}, nil
}

// ─── worktree_list / show ─────────────────────────────────────────

type worktreeListIn struct {
	Repo string `json:"repo,omitempty"`
}
type worktreeRow struct {
	ID        int64  `json:"id"`
	RepoPath  string `json:"repo_path"`
	Path      string `json:"path"`
	Slug      string `json:"slug"`
	Branch    string `json:"branch,omitempty"`
	CreatedAt int64  `json:"created_at_ms"`
}
type worktreeListOut struct {
	Worktrees []worktreeRow `json:"worktrees"`
}

func worktreeListTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeListIn) (*mcpsdk.CallToolResult, worktreeListOut, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, worktreeListOut{}, err
	}
	defer st.Close()
	q := `SELECT w.id, r.path, w.path, w.slug, COALESCE(w.branch,''), w.created_at
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE w.deleted_at IS NULL`
	args := []any{}
	if in.Repo != "" {
		repo, err := resolveRepo(in.Repo)
		if err != nil {
			return nil, worktreeListOut{}, err
		}
		q += " AND r.path = ?"
		args = append(args, repo)
	}
	q += " ORDER BY w.id"
	rows, err := st.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, worktreeListOut{}, err
	}
	defer rows.Close()
	var out []worktreeRow
	for rows.Next() {
		var w worktreeRow
		if err := rows.Scan(&w.ID, &w.RepoPath, &w.Path, &w.Slug, &w.Branch, &w.CreatedAt); err != nil {
			return nil, worktreeListOut{}, err
		}
		out = append(out, w)
	}
	return nil, worktreeListOut{Worktrees: out}, rows.Err()
}

type worktreeShowIn struct {
	Repo string `json:"repo,omitempty"`
	Name string `json:"name" jsonschema:"slug, branch, or basename of the worktree"`
}
type worktreeShowOut struct {
	Worktree worktreeRow   `json:"worktree"`
	Recent   []store.Event `json:"recent_events"`
}

func worktreeShowTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeShowIn) (*mcpsdk.CallToolResult, worktreeShowOut, error) {
	if in.Name == "" {
		return nil, worktreeShowOut{}, fmt.Errorf("name is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, worktreeShowOut{}, err
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, worktreeShowOut{}, err
	}
	defer st.Close()
	var repoID int64
	if err := st.DB.QueryRowContext(ctx, `SELECT id FROM repos WHERE path = ?`, repoRoot).Scan(&repoID); err != nil {
		return nil, worktreeShowOut{}, fmt.Errorf("lookup repo %s: %w", repoRoot, err)
	}
	id, _ := st.LookupWorktreeID(ctx, repoID, in.Name)
	if id == 0 {
		return nil, worktreeShowOut{}, fmt.Errorf("no worktree matches %q", in.Name)
	}
	var w worktreeRow
	w.RepoPath = repoRoot
	if err := st.DB.QueryRowContext(ctx, `SELECT id, slug, COALESCE(branch,''), path, created_at FROM worktrees WHERE id = ?`, id).
		Scan(&w.ID, &w.Slug, &w.Branch, &w.Path, &w.CreatedAt); err != nil {
		return nil, worktreeShowOut{}, err
	}
	events, err := st.QueryEvents(ctx, store.EventFilter{WorktreeID: id, Limit: 50, HydrateWT: false})
	if err != nil {
		return nil, worktreeShowOut{}, err
	}
	return nil, worktreeShowOut{Worktree: w, Recent: events}, nil
}

// ─── logs_query / logs_hooks ──────────────────────────────────────

type logsQueryIn struct {
	Repo        string   `json:"repo,omitempty"`
	Worktree    string   `json:"worktree,omitempty" jsonschema:"slug, branch, or basename"`
	Levels      []string `json:"levels,omitempty" jsonschema:"any of debug|info|warn|error"`
	EventTypes  []string `json:"event_types,omitempty"`
	Phases      []string `json:"phases,omitempty"`
	Since       string   `json:"since,omitempty" jsonschema:"duration (10m, 2h) or RFC3339"`
	PayloadLike string   `json:"payload_like,omitempty"`
	Limit       int      `json:"limit,omitempty" jsonschema:"default 50, max 1000"`
}
type logsQueryOut struct {
	Events []store.Event `json:"events"`
}

func logsQueryTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in logsQueryIn) (*mcpsdk.CallToolResult, logsQueryOut, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	f := store.EventFilter{
		Levels:      validateLevels(in.Levels),
		EventTypes:  in.EventTypes,
		Phases:      in.Phases,
		PayloadLike: in.PayloadLike,
		HydrateWT:   true,
		Limit:       limit,
	}
	if in.Since != "" {
		t, err := parseSince(in.Since)
		if err != nil {
			return nil, logsQueryOut{}, err
		}
		f.SinceMs = t.UnixMilli()
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, logsQueryOut{}, err
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
				return nil, logsQueryOut{}, fmt.Errorf("no worktree matches %q", in.Worktree)
			}
			f.WorktreeID = wid
		}
	}
	events, err := st.QueryEvents(ctx, f)
	if err != nil {
		return nil, logsQueryOut{}, err
	}
	for i := range events {
		events[i].Message = redactSecrets(events[i].Message)
		events[i].PayloadJSON = redactSecrets(events[i].PayloadJSON)
	}
	return nil, logsQueryOut{Events: events}, nil
}

func validateLevels(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	allowed := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(v)
		if allowed[v] {
			out = append(out, v)
		}
	}
	return out
}

func parseSince(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	for _, f := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised since %q", s)
}

func lookupRepoID(ctx context.Context, st *store.Store, root string) (int64, error) {
	var id int64
	row := st.DB.QueryRowContext(ctx, `SELECT id FROM repos WHERE path = ?`, root)
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

type logsHooksIn struct {
	Repo     string `json:"repo,omitempty"`
	Worktree string `json:"worktree" jsonschema:"slug, branch, or basename"`
	Limit    int    `json:"limit,omitempty" jsonschema:"default 50"`
}
type logsHooksOut struct {
	Runs []store.HookRun `json:"runs"`
}

func logsHooksTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in logsHooksIn) (*mcpsdk.CallToolResult, logsHooksOut, error) {
	if in.Worktree == "" {
		return nil, logsHooksOut{}, fmt.Errorf("worktree is required")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, logsHooksOut{}, err
	}
	defer st.Close()
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, logsHooksOut{}, err
	}
	repoID, err := lookupRepoID(ctx, st, repoRoot)
	if err != nil {
		return nil, logsHooksOut{}, fmt.Errorf("lookup repo %s: %w", repoRoot, err)
	}
	wid, _ := st.LookupWorktreeID(ctx, repoID, in.Worktree)
	if wid == 0 {
		return nil, logsHooksOut{}, fmt.Errorf("no worktree matches %q", in.Worktree)
	}
	runs, err := st.QueryHookRuns(ctx, wid, limit)
	if err != nil {
		return nil, logsHooksOut{}, err
	}
	for i := range runs {
		runs[i].StdoutTail = redactSecrets(runs[i].StdoutTail)
		runs[i].StderrTail = redactSecrets(runs[i].StderrTail)
	}
	return nil, logsHooksOut{Runs: runs}, nil
}

// ─── fw_detect / slug_compute / daemon_status ─────────────────────

type fwDetectIn struct {
	Repo string `json:"repo,omitempty"`
}
type fwDetectOut struct {
	Repo            string           `json:"repo"`
	Migration       []framework.Spec `json:"migration"`
	Test            []testfw.Spec    `json:"test"`
	AutoCloneTarget uint32           `json:"auto_clone_target"`
}

func fwDetectTool(_ context.Context, _ *mcpsdk.CallToolRequest, in fwDetectIn) (*mcpsdk.CallToolResult, fwDetectOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, fwDetectOut{}, err
	}
	return nil, fwDetectOut{
		Repo:            repoRoot,
		Migration:       framework.DefaultRegistry().DetectAll(repoRoot),
		Test:            testfw.DetectAll(repoRoot),
		AutoCloneTarget: testfw.DetectedCloneCount(repoRoot),
	}, nil
}

type slugComputeIn struct {
	Path string `json:"path,omitempty" jsonschema:"defaults to cwd"`
}
type slugComputeOut struct {
	Slug            string `json:"slug"`
	Path            string `json:"path"`
	Branch          string `json:"branch,omitempty"`
	RedisQueueIndex int    `json:"redis_queue_index"`
	RedisCacheIndex int    `json:"redis_cache_index"`
}

func slugComputeTool(_ context.Context, _ *mcpsdk.CallToolRequest, in slugComputeIn) (*mcpsdk.CallToolResult, slugComputeOut, error) {
	wt, branch := resolveWorktree(in.Path)
	sl := slug.For(wt, branch)
	q, ca := sl.RedisIndices()
	return nil, slugComputeOut{
		Slug:            sl.Value,
		Path:            wt,
		Branch:          branch,
		RedisQueueIndex: int(q),
		RedisCacheIndex: int(ca),
	}, nil
}

type daemonStatusIn struct{}
type daemonStatusOut struct {
	Status   string `json:"status"`
	Version  string `json:"version,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Watchers int    `json:"watchers,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ─── snapshots_list ───────────────────────────────────────────────

type snapshotsListIn struct {
	Repo string `json:"repo,omitempty"`
}
type snapshotsListRow struct {
	Fingerprint  string `json:"fingerprint"`
	Engine       string `json:"engine"`
	TemplateName string `json:"template_name"`
	SourceDB     string `json:"source_db"`
}
type snapshotsListOut struct {
	Repo      string             `json:"repo"`
	Snapshots []snapshotsListRow `json:"snapshots"`
}

func snapshotsListTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in snapshotsListIn) (*mcpsdk.CallToolResult, snapshotsListOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, snapshotsListOut{}, err
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, snapshotsListOut{}, err
	}
	defer st.Close()
	repoID, err := lookupRepoID(ctx, st, repoRoot)
	if err != nil {
		return nil, snapshotsListOut{Repo: repoRoot}, nil
	}
	cands, err := st.ListSnapshotsForRepo(ctx, repoID)
	if err != nil {
		return nil, snapshotsListOut{}, err
	}
	rows := make([]snapshotsListRow, 0, len(cands))
	for _, c := range cands {
		rows = append(rows, snapshotsListRow{
			Fingerprint:  c.Fingerprint,
			Engine:       c.Engine,
			TemplateName: c.TemplateName,
			SourceDB:     c.SourceDB,
		})
	}
	return nil, snapshotsListOut{Repo: repoRoot, Snapshots: rows}, nil
}

func daemonStatusTool(ctx context.Context, _ *mcpsdk.CallToolRequest, _ daemonStatusIn) (*mcpsdk.CallToolResult, daemonStatusOut, error) {
	resp, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodStatus})
	if err != nil {
		return nil, daemonStatusOut{Status: "not-running", Error: err.Error()}, nil
	}
	if resp.Kind == rpc.KindError {
		return nil, daemonStatusOut{Status: "error", Error: resp.Message}, nil
	}
	return nil, daemonStatusOut{
		Status:   "running",
		Version:  resp.DaemonVersion,
		PID:      int(resp.Pid),
		Watchers: int(resp.WatcherCount),
	}, nil
}
