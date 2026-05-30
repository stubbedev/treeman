package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/schema"
	"github.com/stubbedev/treeman/internal/store"
)

// registerResources exposes static + read-on-demand resources under
// the `treeman://` scheme so clients can attach config + recent logs
// to context without invoking a tool. Templates use a {placeholder}
// in the URI to let the client pick a parameter.
func registerResources(srv *mcpsdk.Server) {
	srv.AddResource(&mcpsdk.Resource{
		URI:         "treeman://config/raw",
		Name:        "treeman.yaml (raw)",
		Description: "The current repo's .treeman.yaml on disk, byte-for-byte. Returns 404 if no repo is detected from cwd or the file is missing.",
		MIMEType:    "application/yaml",
	}, rawConfigResource)

	srv.AddResource(&mcpsdk.Resource{
		URI:         "treeman://config/resolved",
		Name:        "Resolved config",
		Description: "The .treeman.yaml after env/secret substitution + defaults, marshalled as YAML. Use this to see what treeman actually executes against.",
		MIMEType:    "application/yaml",
	}, resolvedConfigResource)

	srv.AddResource(&mcpsdk.Resource{
		URI:         "treeman://config/schema",
		Name:        "Config JSON Schema",
		Description: "The JSON Schema for .treeman.yaml, generated via reflection from config.Config. Use this to validate a config before writing it.",
		MIMEType:    "application/json",
	}, schemaResource)

	srv.AddResource(&mcpsdk.Resource{
		URI:         "treeman://logs/recent",
		Name:        "Recent events (200)",
		Description: "The 200 most recent event-log rows across every repo + worktree, oldest-first. One JSON object per line (NDJSON).",
		MIMEType:    "application/x-ndjson",
	}, recentLogsResource)

	srv.AddResourceTemplate(&mcpsdk.ResourceTemplate{
		URITemplate: "treeman://worktrees/{slug}/events",
		Name:        "Worktree events",
		Description: "The 200 most recent events for one worktree (resolved by slug, branch, or basename). Attach this as context when diagnosing a specific worktree to avoid pulling unrelated noise from logs/recent.",
		MIMEType:    "application/x-ndjson",
	}, worktreeEventsResource)

	srv.AddResourceTemplate(&mcpsdk.ResourceTemplate{
		URITemplate: "treeman://worktrees/{slug}/hooks",
		Name:        "Worktree hook runs",
		Description: "The 50 most recent hook_run rows for one worktree. Use this to see what setup/teardown commands ran, their exit codes, and their stdout/stderr tails.",
		MIMEType:    "application/json",
	}, worktreeHooksResource)

	srv.AddResource(&mcpsdk.Resource{
		URI:         "treeman://daemon/state",
		Name:        "Daemon live state",
		Description: "Live runtime view of treemand: in-flight prepares/teardowns, watcher set, per-repo backoff timers. Same data as the daemon_state tool but attachable as context (cheaper than re-invoking each turn).",
		MIMEType:    "application/json",
	}, daemonStateResource)

	srv.AddResourceTemplate(&mcpsdk.ResourceTemplate{
		URITemplate: "treeman://repos/{repo}/snapshots",
		Name:        "Repo snapshot cache",
		Description: "Cached snapshots (template DBs) for one repo, capped at 100. {repo} is the URL-encoded absolute repo path; use 'cwd' to mean the current dir's repo. Cheaper than calling snapshots_list every turn.",
		MIMEType:    "application/json",
	}, repoSnapshotsResource)

	srv.AddResourceTemplate(&mcpsdk.ResourceTemplate{
		URITemplate: "treeman://repos/{repo}/branches",
		Name:        "Repo branches",
		Description: "Local + origin-only branches for one repo, annotated with worktree occupancy. {repo} is the URL-encoded absolute repo path; use 'cwd' to mean the current dir's repo.",
		MIMEType:    "application/json",
	}, repoBranchesResource)
}

func rawConfigResource(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	repoRoot, err := resolveRepo("")
	if err != nil {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	p := filepath.Join(repoRoot, ".treeman.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/yaml", Text: string(b)}},
	}, nil
}

func resolvedConfigResource(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	repoRoot, err := resolveRepo("")
	if err != nil {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("load resolved: %w", err)
	}
	b, err := json.MarshalIndent(map[string]any{
		"repo":     repoRoot,
		"config":   cfg,
		"resolved": resolve.Resolve(&cfg, repoRoot),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	// Scrub embedded credentials before exposing to LLM clients.
	clean := redactSecrets(string(b))
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: clean}},
	}, nil
}

func schemaResource(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	b, err := schema.Render()
	if err != nil {
		return nil, err
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}},
	}, nil
}

// parseWorktreeSlugURI extracts {slug} from `treeman://worktrees/<slug>/<tail>`.
// Returns (slug, tail, true) on a clean match. `tail` is the remainder
// after the slug — used to dispatch between the events / hooks
// handlers when one handler covers multiple URI shapes (it doesn't
// today, but keeps the helper general).
func parseWorktreeSlugURI(uri string) (slug, tail string, ok bool) {
	const prefix = "treeman://worktrees/"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", false
	}
	rest := uri[len(prefix):]
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", "", false
	}
	slug = parts[0]
	if len(parts) == 2 {
		tail = parts[1]
	}
	return slug, tail, true
}

func worktreeEventsResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	slug, _, ok := parseWorktreeSlugURI(req.Params.URI)
	if !ok || slug == "" {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	wid, _ := st.LookupWorktreeID(ctx, 0, slug)
	if wid == 0 {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	events, err := st.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wid, Limit: 200, HydrateWT: true, OldestFirst: true,
	})
	if err != nil {
		return nil, err
	}
	var buf []byte
	for _, e := range events {
		e.Message = redactSecrets(e.Message)
		e.PayloadJSON = redactSecrets(e.PayloadJSON)
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/x-ndjson", Text: string(buf)}},
	}, nil
}

func worktreeHooksResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	slug, _, ok := parseWorktreeSlugURI(req.Params.URI)
	if !ok || slug == "" {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	wid, _ := st.LookupWorktreeID(ctx, 0, slug)
	if wid == 0 {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	rows, err := st.QueryHookRuns(ctx, wid, 50)
	if err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(map[string]any{
		"worktree_id": wid,
		"slug":        slug,
		"hook_runs":   rows,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)}},
	}, nil
}

// daemonStateResource is the resource counterpart to daemon_state.
func daemonStateResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	body, err := json.MarshalIndent(collectDaemonState(ctx), "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)}},
	}, nil
}

// repoSnapshotsResource returns the cached-snapshot list for one repo.
// {repo} placeholder accepts the literal "cwd" (or empty) to mean
// "resolve from current working dir" so an interactive client doesn't
// need to URL-encode an absolute path.
func repoSnapshotsResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	repo, ok := parseRepoResourceURI(req.Params.URI, "/snapshots")
	if !ok {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	out, err := collectRepoSnapshots(ctx, repo, 100)
	if err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)}},
	}, nil
}

// repoBranchesResource returns the branch list for one repo. See
// repoSnapshotsResource for the {repo} placeholder convention.
func repoBranchesResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	repo, ok := parseRepoResourceURI(req.Params.URI, "/branches")
	if !ok {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}
	out, err := collectRepoBranches(ctx, repo, 200)
	if err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)}},
	}, nil
}

// parseRepoResourceURI extracts the {repo} placeholder from
// `treeman://repos/<repo>/<suffix>`. Returns empty repo (meaning "cwd")
// when the placeholder literally equals "cwd" or "" — letting clients
// avoid URL-encoding an absolute path for the common case.
func parseRepoResourceURI(uri, suffix string) (repo string, ok bool) {
	const prefix = "treeman://repos/"
	if !strings.HasPrefix(uri, prefix) {
		return "", false
	}
	rest := uri[len(prefix):]
	if !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	repo = rest[:len(rest)-len(suffix)]
	if repo == "cwd" {
		repo = ""
	}
	return repo, true
}

func recentLogsResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	events, err := st.QueryEvents(ctx, store.EventFilter{Limit: 200, HydrateWT: true, OldestFirst: true})
	if err != nil {
		return nil, err
	}
	var buf []byte
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/x-ndjson", Text: string(buf)}},
	}, nil
}
