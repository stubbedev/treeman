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
	"github.com/stubbedev/treeman/internal/prepare"
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
		Description: "Run treeman health checks. Call this FIRST when investigating any \"why isn't treeman working\" question — covers daemon reachability, .treeman.yaml load, JSON schema install, migration framework detection, and registry/git worktree drift. Returns one result per check (status: ok|warn|fail|skip) plus a remediation hint.",
		Annotations: readOnlyAnno("Treeman doctor", true),
	}, doctorTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_get",
		Description: "Read the .treeman.yaml for the current (or specified) repo. Pass resolved=true to see the post-substitution config treeman will actually execute against (env vars expanded, connection strings rendered). Use this before any config_write/config_set to know the current state.",
		Annotations: readOnlyAnno("Get .treeman.yaml", false),
	}, configGetTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_validate",
		Description: "Parse and validate .treeman.yaml; report the first parse or validation error. Run this after every config_write to confirm the file still loads — config_write itself validates, but config_validate covers manual edits made outside MCP.",
		Annotations: readOnlyAnno("Validate .treeman.yaml", false),
	}, configValidateTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_schema",
		Description: "Return the JSON Schema for .treeman.yaml, generated via reflection from config.Config. Use this to drive autocomplete or validate a proposed config body before calling config_write.",
		Annotations: readOnlyAnno("Config JSON Schema", false),
	}, configSchemaTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_list",
		Description: "List active worktrees from the SQLite registry. Optionally filter by repo path. Use this to discover slugs, branches, and paths before calling worktree_show, worktree_delete, or scoping a logs_query.",
		Annotations: readOnlyAnno("List worktrees", false),
	}, worktreeListTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_show",
		Description: "Show details for one worktree: slug, branch, path, created-at, allocated ports, branch_scoped active-namespace state (which branch's data occupies each), and the most recent finalize event. Use this to confirm a worktree exists and reached finalize before driving prepare_run or hook_run against it.",
		Annotations: readOnlyAnno("Show worktree", false),
	}, worktreeShowTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "branch_scoped_status",
		Description: "Inspect a worktree's branch_scoped databases. For each: the active namespace (DB name / key prefix the app connects to), which branch's data currently occupies it, and which local git branches have a durable copy a swap could resume from. Connects each engine to probe the deterministic durable names. Use to answer \"what branches can I resume?\" or to debug why a swap re-seeded instead of resuming. No-op (empty) when no databases are branch_scoped.",
		Annotations: readOnlyAnno("Branch-scoped status", true),
	}, branchScopedStatusTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "logs_query",
		Description: "Query the SQLite event log. The PRIMARY tool for diagnosing anything that happened in treeman — every prepare/finalize/teardown/hook/watcher action emits events. All filters are optional and AND-combined: levels, event_types, phases, since (10m|2h|RFC3339), payload_like, run_id (8-char correlation id stamped on every event from one flow). When watching a long-running prepare, prefer logs_wait over polling this.",
		Annotations: readOnlyAnno("Query event log", false),
	}, logsQueryTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "logs_hooks",
		Description: "List the most recent hook_run rows for one worktree (resolved by slug, branch, or basename). Each row carries the hook command, exit code, and stdout/stderr tails. Pair with hook_log_read when a tail isn't enough.",
		Annotations: readOnlyAnno("Hook run history", false),
	}, logsHooksTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "fw_detect",
		Description: "Detect migration and test frameworks for the current (or specified) repo. Call this BEFORE init_repo to know which scaffold template will be used. Returns the same data as `treeman fw detect --json`.",
		Annotations: readOnlyAnno("Detect frameworks", true),
	}, fwDetectTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "slug_compute",
		Description: "Compute the slug treeman would derive for a worktree path. Call this before worktree_create to know which database/redis prefix/index suffix the new worktree will get.",
		Annotations: readOnlyAnno("Compute slug", false),
	}, slugComputeTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "daemon_status",
		Description: "Probe the running treemand for version, PID, and watcher count. Returns status=running when the socket answers, status=not-running when it doesn't. Use this before any daemon-backed action (prepare_run, worktree_create) to confirm the daemon is alive.",
		Annotations: readOnlyAnno("Daemon status", true),
	}, daemonStatusTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "snapshots_list",
		Description: "List cached snapshots (template DBs) for the current (or specified) repo. Use this BEFORE snapshots_purge to see what would be wiped. For per-snapshot drilldown, follow up with snapshot_inspect on each fingerprint.",
		Annotations: readOnlyAnno("List snapshots", false),
	}, snapshotsListTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "logs_wait",
		Description: "Block until at least min_count new events match the supplied filter, or timeout_seconds elapses. Use this instead of polling logs_query when you're watching a long prepare/finalize/teardown to surface its outcome — pair with run_id to scope the wait to one flow's events. Returns the matching events; on timeout returns whatever arrived plus a timed_out=true flag.",
		Annotations: readOnlyAnno("Wait for events", false),
	}, logsWaitTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "branches_list",
		Description: "List local + remote-only git branches for the current (or specified) repo, annotated with whether each branch occupies a live worktree. Call this before worktree_create to pick an unoccupied branch — the same data the CLI's `treeman branches` shows.",
		Annotations: readOnlyAnno("List branches", true),
	}, branchesListTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_diff",
		Description: "Diff a proposed .treeman.yaml body against the current resolved config. Returns added/removed/changed paths so you can preview the effect of config_write before committing. Body is parsed and validated first; a parse error short-circuits the diff.",
		Annotations: readOnlyAnno("Diff config", false),
	}, configDiffTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "inputs_fingerprint",
		Description: "Compute the current snapshot fingerprint for a worktree's databases — the per-input hash breakdown plus whether a matching cached snapshot exists. Use this to answer \"why did this prepare cold-build instead of cache-hit?\": compare the returned input_hashes against the cached_snapshot fields. Omit db_index to report on every database, or pass an integer index to scope. Set probe_engine=true to fetch the live engine version (otherwise the fingerprint won't match the cached one).",
		Annotations: readOnlyAnno("Inspect input hashes", true),
	}, inputsFingerprintTool)

	registerEngineReadTools(srv)
	registerSyncReadTools(srv)
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
	modelineDetail := ""
	if ref != "" {
		if ok, detail := schema.ProbeRef(repoRoot, ref); ok {
			return doctorResult{Name: "schema", Status: "ok", Detail: "modeline → " + detail}
		} else {
			modelineDetail = detail
		}
	}

	if gp, err := schema.GlobalPath(); err == nil {
		if _, err := os.Stat(gp); err == nil {
			return doctorResult{Name: "schema", Status: "ok", Detail: "global install → " + gp}
		}
	}
	repoPath := filepath.Join(repoRoot, "schemas", "treeman.schema.json")
	if _, err := os.Stat(repoPath); err == nil {
		return doctorResult{Name: "schema", Status: "ok", Detail: "repo file → " + repoPath}
	}
	if modelineDetail != "" {
		return doctorResult{Name: "schema", Status: "warn", Detail: "modeline unresolved: " + modelineDetail, Hint: "schema_install"}
	}
	return doctorResult{Name: "schema", Status: "warn", Detail: "no schema installed", Hint: "schema_install"}
}

func checkFrameworks(repoRoot string) doctorResult {
	cfg, _ := resolve.LoadResolved(repoRoot)
	detected := framework.RegistryFor(&cfg).DetectAll(repoRoot)
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
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `SELECT w.path FROM worktrees w
		JOIN repos r ON r.id = w.repo_id WHERE r.path = ? AND w.deleted_at IS NULL`, repoRoot)
	if err != nil {
		return doctorResult{Name: "registry", Status: "fail", Detail: err.Error()}
	}
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = st.Close() }()
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
	defer func() { _ = rows.Close() }()
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
	Name string `json:"name,omitempty" jsonschema:"slug, branch, or basename of the worktree; omit to resolve the worktree containing the current directory"`
}
type worktreeShowOut struct {
	Worktree worktreeRow       `json:"worktree"`
	Ports    map[string]uint16 `json:"ports,omitempty"`
	// BranchScoped lists each branch_scoped database's active namespace
	// and which branch's data currently occupies it — the swap state an
	// agent needs to reason about resume/seed behaviour.
	BranchScoped []store.ActiveBranchRow `json:"branch_scoped,omitempty"`
	Recent       []store.Event           `json:"recent_events"`
}

func worktreeShowTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeShowIn) (*mcpsdk.CallToolResult, worktreeShowOut, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, worktreeShowOut{}, err
	}
	defer func() { _ = st.Close() }()

	var w worktreeRow
	var id int64
	if in.Name == "" {
		// No name — resolve the worktree containing the current dir.
		row, rerr := worktreeRowFromCwd(ctx, st)
		if rerr != nil {
			return nil, worktreeShowOut{}, rerr
		}
		id = row.ID
		w = row
	} else {
		repoRoot, rerr := resolveRepo(in.Repo)
		if rerr != nil {
			return nil, worktreeShowOut{}, rerr
		}
		var repoID int64
		if err := st.DB.QueryRowContext(ctx, `SELECT id FROM repos WHERE path = ?`, repoRoot).Scan(&repoID); err != nil {
			return nil, worktreeShowOut{}, fmt.Errorf("lookup repo %s: %w", repoRoot, err)
		}
		id, _ = st.LookupWorktreeID(ctx, repoID, in.Name)
		if id == 0 {
			return nil, worktreeShowOut{}, fmt.Errorf("no worktree matches %q", in.Name)
		}
		w.RepoPath = repoRoot
		if err := st.DB.QueryRowContext(ctx, `SELECT id, slug, COALESCE(branch,''), path, created_at FROM worktrees WHERE id = ?`, id).
			Scan(&w.ID, &w.Slug, &w.Branch, &w.Path, &w.CreatedAt); err != nil {
			return nil, worktreeShowOut{}, err
		}
	}

	ports, _ := st.LoadWorktreePorts(ctx, id)
	branchScoped, _ := st.ListActiveBranches(ctx, id)
	events, err := st.QueryEvents(ctx, store.EventFilter{WorktreeID: id, Limit: 50, HydrateWT: false})
	if err != nil {
		return nil, worktreeShowOut{}, err
	}
	return nil, worktreeShowOut{Worktree: w, Ports: ports, BranchScoped: branchScoped, Recent: events}, nil
}

// ─── branch_scoped_status ─────────────────────────────────────────

type branchScopedStatusIn struct {
	Worktree string `json:"worktree,omitempty" jsonschema:"defaults to cwd's worktree"`
	Repo     string `json:"repo,omitempty"`
}
type branchScopedStatusOut struct {
	Databases []prepare.BranchScopedDB `json:"databases"`
}

func branchScopedStatusTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in branchScopedStatusIn) (*mcpsdk.CallToolResult, branchScopedStatusOut, error) {
	dbs, err := runBranchScopedStatus(ctx, in.Worktree, in.Repo)
	if err != nil {
		return nil, branchScopedStatusOut{}, err
	}
	return nil, branchScopedStatusOut{Databases: dbs}, nil
}

// worktreeRowFromCwd resolves the worktree row containing the current
// working directory by walking parent dirs. Mirrors the CLI's
// argument-free `wt show`.
func worktreeRowFromCwd(ctx context.Context, st *store.Store) (worktreeRow, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return worktreeRow{}, err
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return worktreeRow{}, err
	}
	for {
		row, err := st.LookupActiveWorktreeByPath(ctx, dir)
		if err != nil {
			return worktreeRow{}, err
		}
		if row.ID != 0 {
			return worktreeRow{
				ID:     row.ID,
				Slug:   row.Slug,
				Branch: row.Branch,
				Path:   row.Path,
			}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return worktreeRow{}, fmt.Errorf("not inside a tracked worktree (run from inside one, or pass a worktree name — see `treeman wt list`)")
		}
		dir = parent
	}
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
	RunID       string   `json:"run_id,omitempty" jsonschema:"exact correlation id (8-char hex stamped on every event from one prepare/finalize/teardown/watcher flow)"`
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
		RunID:       in.RunID,
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
	defer func() { _ = st.Close() }()
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
	defer func() { _ = st.Close() }()
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
	defer func() { _ = st.Close() }()
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
