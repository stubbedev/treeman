package mcp

import (
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// jsonAnyTypes is the explicit JSON type set for a tool field that accepts any
// value. A Go `any`/`interface{}` (or `map[string]any`) field reflects to an
// empty subschema, which jsonschema-go marshals as the boolean `true`. Claude
// Code's MCP client validates the ENTIRE tools/list payload and rejects any
// property schema that is the boolean `true` (or otherwise lacks a "type") —
// and one offending property fails the whole list, so every treeman tool
// vanishes and the client reports "tool fetch failed". (The initialize
// handshake still succeeds, so server *instructions* load while *tools* do
// not — that split is the diagnostic fingerprint.) Pinning this union keeps
// "accept anything" semantics while producing a schema Claude Code accepts.
//
// `additionalProperties: false` (marshaled from a non-empty schema) is NOT
// affected and is accepted by Claude Code — pinAnyTypes deliberately leaves it
// alone.
var jsonAnyTypes = []string{"null", "boolean", "number", "string", "array", "object"}

// addTool is the registration entry point for EVERY tool. It does not
// register immediately — it records the tool into the build catalog so
// newServer can decide whether to advertise it up front (core) or defer
// it behind the `tools` gateway (see catalog.go). The actual pinned
// registration happens via registerTool when the tool is activated.
func addTool[In, Out any](s *mcpsdk.Server, t *mcpsdk.Tool, h mcpsdk.ToolHandlerFor[In, Out]) {
	recordTool(t, func() { registerTool(s, t, h) })
}

// registerTool pre-builds the input/output schemas, pins an explicit type
// union on every typeless ("any") subschema, then registers with the SDK.
// Pre-setting the schemas makes the SDK skip its own reflection (which
// would emit the boolean nodes Claude Code rejects). The single chokepoint
// where a tool actually enters the server — used both for activating core
// tools and for the gateway revealing deferred ones.
func registerTool[In, Out any](s *mcpsdk.Server, t *mcpsdk.Tool, h mcpsdk.ToolHandlerFor[In, Out]) {
	if t.InputSchema == nil {
		// Mirror the SDK: a bare `any` input is an empty object, not "any value".
		if reflect.TypeFor[In]() == reflect.TypeFor[any]() {
			t.InputSchema = &jsonschema.Schema{Type: "object"}
		} else {
			t.InputSchema = schemaFor[In]()
		}
	}
	// The SDK only derives an output schema when Out is a concrete type; match
	// that so `any`-returning tools stay output-schema-free.
	if t.OutputSchema == nil && reflect.TypeFor[Out]() != reflect.TypeFor[any]() {
		t.OutputSchema = schemaFor[Out]()
	}
	mcpsdk.AddTool(s, t, h)
}

// schemaFor reflects T into a JSON schema and pins an explicit type union on
// every typeless ("any") subschema.
func schemaFor[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic("mcp: build schema for tool: " + err.Error())
	}
	pinAnyTypes(s)
	return s
}

// pinAnyTypes walks s and replaces every typeless leaf with the jsonAnyTypes
// union. A node is "typeless" when it carries no type and no structural or
// composition keyword that would otherwise constrain it (a description alone
// does not constrain). It deliberately does NOT recurse into `Not`: the `false`
// boolean schema (used by additionalProperties:false) is modeled as
// `{Not: {}}`, and pinning its empty child would turn `false` into a typed
// object — changing its meaning.
func pinAnyTypes(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if isTypelessAny(s) {
		s.Types = jsonAnyTypes
	}
	for _, c := range s.Properties {
		pinAnyTypes(c)
	}
	for _, c := range s.PatternProperties {
		pinAnyTypes(c)
	}
	for _, c := range s.Defs {
		pinAnyTypes(c)
	}
	pinAnyTypes(s.AdditionalProperties)
	pinAnyTypes(s.PropertyNames)
	pinAnyTypes(s.Items)
	for _, c := range s.ItemsArray {
		pinAnyTypes(c)
	}
	for _, c := range s.PrefixItems {
		pinAnyTypes(c)
	}
	pinAnyTypes(s.AdditionalItems)
	pinAnyTypes(s.Contains)
	for _, c := range s.AllOf {
		pinAnyTypes(c)
	}
	for _, c := range s.AnyOf {
		pinAnyTypes(c)
	}
	for _, c := range s.OneOf {
		pinAnyTypes(c)
	}
}

func isTypelessAny(s *jsonschema.Schema) bool {
	return s.Type == "" && len(s.Types) == 0 &&
		s.Ref == "" && s.Const == nil && len(s.Enum) == 0 &&
		len(s.Properties) == 0 && s.AdditionalProperties == nil &&
		s.Items == nil && len(s.ItemsArray) == 0 && len(s.PrefixItems) == 0 &&
		len(s.AllOf) == 0 && len(s.AnyOf) == 0 && len(s.OneOf) == 0 &&
		s.Not == nil && s.Format == ""
}
