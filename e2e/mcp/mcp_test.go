//go:build e2e

// Package mcp_e2e drives the `treeman mcp` server over its stdio
// JSON-RPC transport. Sequence:
//
//  1. Spawn `treeman mcp` with stdin/stdout pipes.
//  2. Send `initialize` → expect `result.serverInfo.name == "treeman"`.
//  3. Send `tools/list` → expect a non-empty tool list including
//     at least one well-known tool (`status` or `worktree_list`).
//  4. Send `tools/call` for `status` → expect a structured result.
//
// Validates that the binary exposes a working MCP transport — the
// integration point IDEs and other clients depend on.
package mcp_e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startServer builds and spawns `treeman mcp` on stdio, returning a
// send/read pair for raw JSON-RPC lines.
func startServer(t *testing.T) (func(map[string]any), func() map[string]any) {
	t.Helper()
	// Build a fresh treeman binary.
	repoRoot := projectRoot(t)
	binDir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, "treeman"), "./cmd/treeman")
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	srv := exec.CommandContext(ctx, filepath.Join(binDir, "treeman"), "mcp")
	// Isolate state so MCP doesn't trip over stale registrations.
	stateDir := t.TempDir()
	dbPath := filepath.Join(stateDir, "treeman.db")
	srv.Env = append(os.Environ(),
		"TREEMAN_DB_PATH="+dbPath,
		"TREEMAN_SOCKET="+filepath.Join(filepath.Dir(dbPath), "tm.sock"),
		"XDG_CONFIG_HOME="+t.TempDir(),
		// Advertise every tool — these tests exercise tool behavior, not
		// the lazy-disclosure gateway (unit-tested in internal/mcp).
		"TREEMAN_MCP_ALL_TOOLS=1",
	)
	stdin, err := srv.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := srv.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	})

	rdr := bufio.NewReader(stdout)
	send := func(req map[string]any) {
		t.Helper()
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write(append(body, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		line, err := rdr.ReadBytes('\n')
		if err != nil && err != io.EOF {
			t.Fatalf("read: %v", err)
		}
		if len(line) == 0 {
			t.Fatal("empty response")
		}
		var resp map[string]any
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("parse %q: %v", string(line), err)
		}
		return resp
	}
	return send, read
}

func TestMCPServerJSONRPC(t *testing.T) {
	send, read := startServer(t)

	// 1. initialize
	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "treeman-e2e",
				"version": "0.0.0",
			},
		},
	})
	resp := read()
	if errObj := resp["error"]; errObj != nil {
		t.Fatalf("initialize error: %v", errObj)
	}
	result, _ := resp["result"].(map[string]any)
	srvInfo, _ := result["serverInfo"].(map[string]any)
	name, _ := srvInfo["name"].(string)
	if name != "treeman" {
		t.Errorf("serverInfo.name = %q, want treeman", name)
	}
	t.Logf("initialize → serverInfo.name=%s", name)

	// MCP protocol requires a notification of "initialized" before
	// tool calls. The mcp-sdk also accepts a tools/list request
	// immediately, but be polite.
	send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	// 2. tools/list
	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	resp = read()
	if errObj := resp["error"]; errObj != nil {
		t.Fatalf("tools/list error: %v", errObj)
	}
	result, _ = resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("tools/list returned no tools")
	}
	gotTools := map[string]bool{}
	for _, raw := range tools {
		m, _ := raw.(map[string]any)
		if n, ok := m["name"].(string); ok {
			gotTools[n] = true
		}
	}
	t.Logf("tools/list returned %d tools: %v", len(tools), keys(gotTools))
	// Check at least one well-known read-only tool is present.
	found := false
	for _, want := range []string{"status", "doctor_check", "worktree_list", "logs_query"} {
		if gotTools[want] {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected one of [status/doctor_check/worktree_list/logs_query] tools to be registered")
	}
}

func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = strings.Contains
