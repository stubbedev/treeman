package mcp

// CatalogEntry is one tool or prompt in the MCP catalog, exported for
// doc generation (cmd/treeman-gen-mcp-docs) so the tool reference is
// rendered straight from the registry instead of hand-maintained.
type CatalogEntry struct {
	Name     string
	Category string
	Summary  string
	Core     bool
	IsPrompt bool
}

// Catalog builds the MCP server and returns every registered tool +
// prompt as a flat catalog (name, category, one-line summary). The
// single source of truth behind docs/mcp-tools.md.
func Catalog() []CatalogEntry {
	_, cat := buildServer()
	out := make([]CatalogEntry, 0, len(cat))
	for _, e := range cat {
		out = append(out, CatalogEntry{
			Name:     e.name,
			Category: e.category,
			Summary:  e.summary,
			Core:     e.core,
			IsPrompt: e.category == "prompt",
		})
	}
	return out
}
