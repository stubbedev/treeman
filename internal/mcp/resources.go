package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/resolve"
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
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}},
	}, nil
}

func schemaResource(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	b, err := renderConfigSchema()
	if err != nil {
		return nil, err
	}
	return &mcpsdk.ReadResourceResult{
		Contents: []*mcpsdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}},
	}, nil
}

func recentLogsResource(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
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
