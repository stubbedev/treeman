package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// allTools is the complete set of registerable tools (core + deferred),
// the single source of truth the tests guard against drops/dupes.
var allTools = []string{
	// read
	"doctor", "config_get", "config_validate", "config_schema",
	"config_locate",
	"worktree_list", "worktree_show", "branch_scoped_status",
	"logs_query", "logs_hooks", "fw_detect", "slug_compute",
	"daemon_status", "daemon_state",
	"snapshots_list", "logs_wait", "logs_subscribe", "branches_list",
	"config_diff", "config_history", "inputs_fingerprint", "prepare_dry_run",
	"prompts_list",
	// engine
	"engine_status", "db_schema_dump", "db_query", "snapshots_inspect",
	"hook_log_read", "snapshots_drop", "db_dump", "connection_probe",
	"engine_logs", "es_request",
	// write
	"prepare_run", "db_reset", "hook_run", "config_write", "config_set",
	"config_unset", "config_delete", "config_restore",
	"registry_register", "registry_unregister", "registry_repair",
	"repo_remove", "worktree_repair",
	"snapshots_purge", "logs_purge",
	"schema_install", "init_repo", "daemon_control",
	"worktree_create", "worktree_delete",
	"worktree_finalize", "main_worktree", "status_overview", "notify_test",
	// sync
	"sync_status", "sync_now",
}

// allPromptNames is every prompt the server can serve (deferred behind
// the gateway like non-core tools).
var allPromptNames = []string{
	"diagnose-prepare-failure", "scaffold-from-framework", "cache-cleanup",
	"worktree-setup", "migration-trial", "edit-config", "bootstrap-new-repo",
}

// TestToolCatalogComplete asserts the full set of registerable entries
// (core + deferred tools, plus prompts under category "prompt") exactly
// matches expectations. Guards against an accidentally-dropped or
// duplicated addTool/prompt and keeps the docs honest.
func TestToolCatalogComplete(t *testing.T) {
	_, cat := buildServer()

	tools, prompts := map[string]bool{}, map[string]bool{}
	for _, e := range cat {
		bucket := tools
		if e.category == "prompt" {
			bucket = prompts
		}
		if bucket[e.name] {
			t.Errorf("%q recorded more than once", e.name)
		}
		bucket[e.name] = true
	}

	assertExactly(t, "tool", tools, allTools)
	assertExactly(t, "prompt", prompts, allPromptNames)
}

func assertExactly(t *testing.T, kind string, got map[string]bool, want []string) {
	t.Helper()
	for _, name := range want {
		if !got[name] {
			t.Errorf("expected %s %q in the catalog", kind, name)
		}
		delete(got, name)
	}
	for extra := range got {
		t.Errorf("unexpected %s in catalog (regression?): %q", kind, extra)
	}
}

// TestCoreToolsAreInCatalog guards that every name in coreTools actually
// maps to a real tool (a typo there would silently demote a tool to
// deferred while claiming it's core).
func TestCoreToolsAreInCatalog(t *testing.T) {
	known := map[string]bool{}
	for _, n := range allTools {
		known[n] = true
	}
	for n := range coreTools {
		if !known[n] {
			t.Errorf("coreTools names a non-existent tool %q", n)
		}
	}
}

// TestLazyAdvertisesCorePlusGateway: with lazy disclosure (default), the
// server advertises exactly the core set plus the `tools` gateway —
// everything else is deferred until the gateway loads it.
func TestLazyAdvertisesCorePlusGateway(t *testing.T) {
	t.Setenv("TREEMAN_MCP_ALL_TOOLS", "")
	advertised := advertisedTools(t, newServer())

	for name := range advertised {
		if name == "tools" {
			continue
		}
		if !coreTools[name] {
			t.Errorf("non-core tool %q advertised up front", name)
		}
	}
	for name := range coreTools {
		if !advertised[name] {
			t.Errorf("core tool %q not advertised", name)
		}
	}
	if !advertised["tools"] {
		t.Error("the `tools` gateway must always be advertised")
	}
}

// TestAllToolsEnvAdvertisesEverything: the escape hatch advertises the
// full surface and omits the gateway.
func TestAllToolsEnvAdvertisesEverything(t *testing.T) {
	t.Setenv("TREEMAN_MCP_ALL_TOOLS", "1")
	advertised := advertisedTools(t, newServer())

	for _, name := range allTools {
		if !advertised[name] {
			t.Errorf("expected %q advertised under TREEMAN_MCP_ALL_TOOLS=1", name)
		}
	}
	if advertised["tools"] {
		t.Error("gateway should not be registered when all tools are advertised")
	}
}

// TestToolGatewayEnable exercises the gateway: enabling by name and by
// category, idempotency, and that list reflects enabled state.
func TestToolGatewayEnable(t *testing.T) {
	_, cat := buildServer()

	out := gatewayEnable(cat, toolsIn{Names: []string{"db_query"}})
	if len(out.Enabled) != 1 || out.Enabled[0] != "db_query" {
		t.Fatalf("enable by name: %+v", out)
	}
	if again := gatewayEnable(cat, toolsIn{Names: []string{"db_query"}}); len(again.Enabled) != 0 {
		t.Errorf("re-enable should be a no-op: %+v", again)
	}
	if cfg := gatewayEnable(cat, toolsIn{Category: "config"}); len(cfg.Enabled) == 0 {
		t.Error("enable by category should load config_* tools")
	}

	list := gatewayList(cat)
	var dbQueryEnabled bool
	for _, e := range list.Categories["db"] {
		if e.Name == "db_query" {
			dbQueryEnabled = e.Enabled
		}
	}
	if !dbQueryEnabled {
		t.Error("list should report db_query as enabled after enabling it")
	}
}

// TestPromptsDeferred: prompts are hidden up front under lazy disclosure
// and advertised in full under the escape hatch.
func TestPromptsDeferred(t *testing.T) {
	t.Setenv("TREEMAN_MCP_ALL_TOOLS", "")
	if n := advertisedPromptCount(t, newServer()); n != 0 {
		t.Errorf("lazy mode should advertise no prompts up front, got %d", n)
	}
	t.Setenv("TREEMAN_MCP_ALL_TOOLS", "1")
	if n := advertisedPromptCount(t, newServer()); n != len(allPromptNames) {
		t.Errorf("escape hatch should advertise all %d prompts, got %d", len(allPromptNames), n)
	}
}

func advertisedPromptCount(t *testing.T, srv *mcpsdk.Server) int {
	t.Helper()
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := cl.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	n := 0
	for _, err := range cs.Prompts(ctx, nil) {
		if err != nil {
			t.Fatalf("list prompts: %v", err)
		}
		n++
	}
	return n
}

func advertisedTools(t *testing.T, srv *mcpsdk.Server) map[string]bool {
	t.Helper()
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	got := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		got[tool.Name] = true
	}
	return got
}
