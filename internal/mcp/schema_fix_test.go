package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestToolSchemasAreClaudeCompatible lists every registered tool and asserts
// that no input/output schema contains a boolean `true` (or otherwise
// typeless) subschema at a schema position. Claude Code's MCP client rejects
// the WHOLE tools/list when any property schema is the boolean `true` — one
// `any`/`map[string]any` field with no pinned type makes the entire treeman
// server fail with "tool fetch failed". addTool/pinAnyTypes exist to prevent
// exactly that; this test fails loudly if a new tool reintroduces it.
func TestToolSchemasAreClaudeCompatible(t *testing.T) {
	ctx := context.Background()

	// Advertise every tool so the schema check covers the deferred ones,
	// not just the core set the lazy gateway loads up front.
	t.Setenv("TREEMAN_MCP_ALL_TOOLS", "1")
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

	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		for _, s := range []struct {
			kind   string
			schema any
		}{{"inputSchema", tool.InputSchema}, {"outputSchema", tool.OutputSchema}} {
			if s.schema == nil {
				continue
			}
			raw, err := json.Marshal(s.schema)
			if err != nil {
				t.Fatalf("%s.%s: marshal: %v", tool.Name, s.kind, err)
			}
			var node any
			if err := json.Unmarshal(raw, &node); err != nil {
				t.Fatalf("%s.%s: unmarshal: %v", tool.Name, s.kind, err)
			}
			walkSchemaPositions(t, tool.Name+"."+s.kind, node)
		}
	}
}

// walkSchemaPositions descends only positions that hold a subschema and flags
// any that is the boolean `true` or a typeless object. `additionalProperties:
// false` (boolean false) is explicitly allowed — Claude Code accepts it.
func walkSchemaPositions(t *testing.T, path string, node any) {
	t.Helper()
	switch v := node.(type) {
	case bool:
		if v {
			t.Errorf("%s: boolean `true` schema — Claude Code rejects this; pin a type", path)
		}
		return
	case map[string]any:
		if isTypelessJSON(v) {
			t.Errorf("%s: typeless schema %v — pin a type union", path, v)
		}
		if props, ok := v["properties"].(map[string]any); ok {
			for k, sub := range props {
				walkSchemaPositions(t, path+"."+k, sub)
			}
		}
		for _, key := range []string{"items", "additionalProperties", "additionalItems", "contains", "propertyNames"} {
			if sub, ok := v[key]; ok {
				walkSchemaPositions(t, path+"."+key, sub)
			}
		}
		for _, key := range []string{"prefixItems", "allOf", "anyOf", "oneOf"} {
			if arr, ok := v[key].([]any); ok {
				for _, sub := range arr {
					walkSchemaPositions(t, path+"."+key, sub)
				}
			}
		}
	}
}

// isTypelessJSON reports whether a marshaled object schema constrains nothing —
// no type, no composition, no structural keyword. Such a node behaves as "any"
// and is what Claude Code rejects once it loses the boolean-`true` shorthand.
func isTypelessJSON(m map[string]any) bool {
	for _, k := range []string{
		"type", "$ref", "const", "enum", "not", "format",
		"properties", "additionalProperties", "items", "prefixItems",
		"allOf", "anyOf", "oneOf",
	} {
		if _, ok := m[k]; ok {
			return false
		}
	}
	return true
}
