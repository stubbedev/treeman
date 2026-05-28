package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServerRegistersExpectedTools connects an in-process client to the
// fully-registered server and asserts the advertised tool set exactly
// matches expectations. Guards against an accidentally-dropped or
// duplicated AddTool (e.g. when wiring a new feature's tool) and keeps
// the docs/mcp.md "Tools exposed" table honest.
func TestServerRegistersExpectedTools(t *testing.T) {
	ctx := context.Background()

	srv := newServer()
	ct, st := mcpsdk.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	got := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		if got[tool.Name] {
			t.Errorf("tool %q registered more than once", tool.Name)
		}
		got[tool.Name] = true
	}

	want := []string{
		// read
		"doctor", "config_get", "config_validate", "config_schema",
		"worktree_list", "worktree_show", "branch_scoped_status",
		"logs_query", "logs_hooks", "fw_detect", "slug_compute",
		"daemon_status", "snapshots_list", "logs_wait", "branches_list",
		"config_diff", "inputs_fingerprint",
		// engine
		"engine_status", "db_schema_dump", "db_query", "snapshot_inspect",
		"hook_log_read", "snapshot_drop", "db_dump",
		// write
		"prepare_run", "db_reset", "hook_run", "config_write", "config_set",
		"registry_register", "registry_unregister", "registry_repair",
		"registry_remove", "snapshots_purge", "logs_purge",
		"schema_install", "init_repo", "daemon_control",
		"worktree_create", "worktree_delete",
		// sync
		"sync_status", "sync_now",
	}

	for _, name := range want {
		if !got[name] {
			t.Errorf("expected tool %q to be registered", name)
		}
		delete(got, name)
	}
	for extra := range got {
		t.Errorf("unexpected tool registered (add to want or a regression): %q", extra)
	}
}
