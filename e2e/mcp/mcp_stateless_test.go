//go:build e2e

package mcp_e2e

import "testing"

// TestMCPStatelessProtocol drives the 2026-07-28 spec revision, which
// drops the `initialize` handshake: every request instead carries its
// protocol version and client capabilities in `_meta`, and every result
// carries `_meta["io.modelcontextprotocol/serverInfo"]` back. Guards
// against a go-sdk bump silently regressing the sessionless path that
// newer clients use.
func TestMCPStatelessProtocol(t *testing.T) {
	send, read := startServer(t)

	// No initialize, no notifications/initialized — straight to a call.
	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				"io.modelcontextprotocol/clientInfo": map[string]any{
					"name":    "treeman-e2e",
					"version": "0.0.0",
				},
			},
		},
	})
	resp := read()
	if errObj := resp["error"]; errObj != nil {
		t.Fatalf("tools/list error: %v", errObj)
	}
	result, _ := resp["result"].(map[string]any)
	if tools, _ := result["tools"].([]any); len(tools) == 0 {
		t.Fatalf("tools/list returned no tools")
	}
	meta, _ := result["_meta"].(map[string]any)
	srvInfo, _ := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if name, _ := srvInfo["name"].(string); name != "treeman" {
		t.Errorf("_meta serverInfo.name = %q, want treeman", name)
	}

	// `initialize` is removed in this revision; sending it with the new
	// _meta must be rejected rather than silently accepted.
	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "initialize",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	})
	if resp := read(); resp["error"] == nil {
		t.Errorf("initialize accepted under 2026-07-28, want error: %v", resp)
	}
}
