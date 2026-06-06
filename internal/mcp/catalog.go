package mcp

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Lazy tool disclosure
// ────────────────────
// Advertising all ~60 tools up front floods the model's context with
// schemas it rarely needs. Instead the server registers only a curated
// CORE set plus one `tools` gateway at startup; the gateway lists the
// rest by category and loads them on demand via AddTool (which fires the
// SDK's tools/list_changed notification). Set TREEMAN_MCP_ALL_TOOLS=1 to
// register every tool up front (the pre-disclosure behavior).
//
// Mechanism: addTool (schema_fix.go) records a toolEntry instead of
// registering immediately; newServer then activates core + gateway. The
// ~60 addTool call sites are unchanged.

// coreTools is the always-loaded entry-point set — the handful a session
// reaches for first, kept small so the up-front context stays lean.
// Everything else (config, engine access, snapshots, registry, daemon
// control, init, the rest of the worktree/logs surface) is one
// `tools` action=enable away.
var coreTools = map[string]bool{
	"doctor":          true, // "what's wrong"
	"status_overview": true, // fleet state
	"worktree_create": true, // the primary verb
	"worktree_list":   true, // what exists
	"prepare_run":     true, // build/refresh a worktree's DBs
	"logs_query":      true, // see what happened
}

// toolEntry is a registerable tool captured during the build phase.
type toolEntry struct {
	name     string
	category string
	summary  string
	core     bool
	register func() // pins schemas + performs the real mcpsdk.AddTool
	enabled  bool
}

var (
	buildMu      sync.Mutex   // serialises a newServer build
	pendingTools []*toolEntry // collected by recordTool during a build
	catMu        sync.Mutex   // guards enabled flags post-build
)

// recordTool appends a tool to the in-progress build catalog instead of
// registering it immediately. Called by addTool.
func recordTool(t *mcpsdk.Tool, register func()) {
	pendingTools = append(pendingTools, &toolEntry{
		name:     t.Name,
		category: categoryOf(t.Name),
		summary:  firstSentence(t.Description),
		core:     coreTools[t.Name],
		register: register,
	})
}

// activateTools registers the tools for a freshly-built server: every
// tool when TREEMAN_MCP_ALL_TOOLS=1, otherwise the core set plus the
// `tools` gateway that reveals the rest on demand. Returns the catalog.
func activateTools(srv *mcpsdk.Server) []*toolEntry {
	cat := pendingTools
	pendingTools = nil
	all := os.Getenv("TREEMAN_MCP_ALL_TOOLS") == "1"
	for _, e := range cat {
		if all || e.core {
			e.register()
			e.enabled = true
		}
	}
	if !all {
		registerToolGateway(srv, cat)
	}
	return cat
}

// categoryOf groups a tool by the prefix before its first underscore
// ("config_get" → "config"), normalising the snapshots/snapshot pair.
func categoryOf(name string) string {
	cat := name
	if i := strings.IndexByte(name, '_'); i > 0 {
		cat = name[:i]
	}
	if cat == "snapshots" {
		cat = "snapshot"
	}
	return cat
}

// firstSentence trims a tool description to its first sentence for the
// catalog listing (the full schema only loads once the tool is enabled).
func firstSentence(desc string) string {
	if i := strings.IndexByte(desc, '.'); i > 0 {
		return desc[:i+1]
	}
	return desc
}

// ─── the `tools` gateway ───────────────────────────────────────────

type toolsIn struct {
	Action   string   `json:"action,omitempty"   jsonschema:"list (default) — catalog every available tool by category with a one-line summary; enable — load named tools (or a whole category) into the session so they become callable"`
	Names    []string `json:"names,omitempty"    jsonschema:"tool names to load (action=enable)"`
	Category string   `json:"category,omitempty" jsonschema:"load every tool in this category, e.g. 'config' or 'db' (action=enable)"`
}

type toolCatalogEntry struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Enabled bool   `json:"enabled"`
}

type toolsOut struct {
	Action     string                        `json:"action"`
	Categories map[string][]toolCatalogEntry `json:"categories,omitempty"`
	Guide      string                        `json:"guide,omitempty"`
	Enabled    []string                      `json:"enabled,omitempty"`
	Hint       string                        `json:"hint"`
}

// toolGuide is the high-leverage routing that used to live in the
// server's initialize instructions. Moved here so it loads on demand
// (when the agent calls action=list) instead of in every session's
// context. Names tools that may need enabling first.
const toolGuide = `Entry points (enable the named tool first if it isn't loaded):
- "why did this fail" → doctor, then logs_query (or the diagnose-prepare-failure prompt).
- "set up a worktree for branch X" → the worktree-setup prompt (branches_list → daemon_status → worktree_create → logs_wait).
- "set up treeman from scratch" → the bootstrap-new-repo prompt.
- "what's the daemon doing" → daemon_state; version/PID only → daemon_status.
- "are engines reachable" → engine_status; probe one connection string → connection_probe.
- "edit config" → config_locate → config_get → config_diff → config_set (surgical) or config_write (full); config_unset/config_delete/config_restore for the rest. scope=repo (.treeman.yaml) | global (~/.config/treeman/config.yaml).
- "why cold-build not cache-hit" → inputs_fingerprint, then snapshot_inspect.
- "what would prepare run" → prepare_dry_run. "stream progress" → logs_subscribe.
- "worktree stuck" → worktree_repair. "re-run setup/prepare" → worktree_finalize.
- "enroll repo root for per-branch DBs" → main_worktree action=enable|disable|status.
- "read/write engine data" → db_query (SQL/Mongo/Redis) or es_request (Elasticsearch); reads free, writes need write=true+ack=true.
- prompts (guided multi-step flows): diagnose-prepare-failure, scaffold-from-framework, cache-cleanup, worktree-setup, migration-trial, edit-config, bootstrap-new-repo — list them with prompts_list.
Destructive tools (worktree_delete, snapshots_purge, snapshot_drop, db_reset, repo_remove, registry_unregister, logs_purge, config_delete, config_unset) take dry_run=true — preview before committing.`

// registerToolGateway installs the always-on `tools` tool that discovers
// and loads the deferred tools. It closes over the build catalog.
func registerToolGateway(srv *mcpsdk.Server, cat []*toolEntry) {
	gw := &mcpsdk.Tool{
		Name:        "tools",
		Description: "Discover + load treeman's deferred tools. Only a core set is loaded up front to keep context lean; this gateway reveals the rest. action=list (default) returns every available tool grouped by category with a one-line summary; action=enable + names=[...] (or category=...) loads their full schemas so they become callable (the client is notified via tools/list_changed). Enable only what the task needs.",
		Annotations: readOnlyAnno("Discover/load tools", false),
	}
	registerTool(srv, gw, func(_ context.Context, _ *mcpsdk.CallToolRequest, in toolsIn) (*mcpsdk.CallToolResult, toolsOut, error) {
		if in.Action == "enable" {
			return nil, gatewayEnable(cat, in), nil
		}
		return nil, gatewayList(cat), nil
	})
}

func gatewayList(cat []*toolEntry) toolsOut {
	catMu.Lock()
	defer catMu.Unlock()
	cats := map[string][]toolCatalogEntry{}
	for _, e := range cat {
		cats[e.category] = append(cats[e.category], toolCatalogEntry{
			Name: e.name, Summary: e.summary, Enabled: e.enabled,
		})
	}
	for _, list := range cats {
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	return toolsOut{
		Action:     "list",
		Categories: cats,
		Guide:      toolGuide,
		Hint:       "call tools with action=enable and names=[...] (or category=...) to load the ones this task needs",
	}
}

func gatewayEnable(cat []*toolEntry, in toolsIn) toolsOut {
	want := make(map[string]bool, len(in.Names))
	for _, n := range in.Names {
		want[n] = true
	}
	catMu.Lock()
	defer catMu.Unlock()
	var enabled []string
	for _, e := range cat {
		if e.enabled {
			continue
		}
		if want[e.name] || (in.Category != "" && e.category == in.Category) {
			e.register()
			e.enabled = true
			enabled = append(enabled, e.name)
		}
	}
	hint := "enabled — now callable (client refreshed via tools/list_changed)"
	if len(enabled) == 0 {
		hint = "nothing matched (already enabled, or unknown name/category — call action=list to see the catalog)"
	}
	return toolsOut{Action: "enable", Enabled: enabled, Hint: hint}
}
