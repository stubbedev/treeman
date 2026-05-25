package mcp

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool-annotation helpers. Clients use these hints to render tools
// (read-only tools group together, destructive tools get a warning,
// etc.) and to decide which to surface in a tight context window.
// We provide them centrally so every tool registration stays one
// line and the hints stay consistent.

// boolPtr is needed because ToolAnnotations uses *bool for
// DestructiveHint and OpenWorldHint — the SDK distinguishes
// unset (nil → default) from explicitly-false.
func boolPtr(b bool) *bool { return &b }

// readOnlyAnno marks a tool as side-effect-free, with a human title.
// openWorld=true means the tool reaches outside treeman's own state
// (engines, git, the filesystem); false means it touches only
// in-process state + SQLite.
func readOnlyAnno(title string, openWorld bool) *mcpsdk.ToolAnnotations {
	return &mcpsdk.ToolAnnotations{
		Title:         title,
		ReadOnlyHint:  true,
		OpenWorldHint: boolPtr(openWorld),
	}
}

// writeAnno marks a tool as state-mutating, with a destructive hint
// and idempotency hint set by the caller. Use destructive=true for
// anything that can lose work (deletes, drops, overwrites); false
// for additive writes (e.g. registry_register). The MCP spec
// recommends clients prompt the user before invoking destructive
// tools.
func writeAnno(title string, destructive, idempotent, openWorld bool) *mcpsdk.ToolAnnotations {
	return &mcpsdk.ToolAnnotations{
		Title:           title,
		DestructiveHint: boolPtr(destructive),
		IdempotentHint:  idempotent,
		OpenWorldHint:   boolPtr(openWorld),
	}
}
