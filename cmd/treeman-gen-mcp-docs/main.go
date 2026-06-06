// Command treeman-gen-mcp-docs renders the MCP tool + prompt reference
// to the file named in os.Args[1], straight from the live tool registry
// (internal/mcp.Catalog). Adding/renaming a tool updates this page with
// no hand-editing. Wired into `just sync-docs`; PRs catch drift.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/stubbedev/treeman/internal/mcp"
)

func main() {
	cat := mcp.Catalog()

	// Group tools by category; collect prompts separately.
	byCat := map[string][]mcp.CatalogEntry{}
	var prompts []mcp.CatalogEntry
	for _, e := range cat {
		if e.IsPrompt {
			prompts = append(prompts, e)
			continue
		}
		byCat[e.Category] = append(byCat[e.Category], e)
	}
	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	var b strings.Builder
	b.WriteString("# MCP tool reference\n\n")
	b.WriteString("[← back to README](../README.md)\n\n")
	b.WriteString("Auto-generated from the MCP tool registry (`internal/mcp.Catalog`).\n")
	b.WriteString("Run `just sync-docs` after adding or renaming a tool to refresh.\n")
	b.WriteString("Tools marked **core** load up-front; the rest are revealed on demand\n")
	b.WriteString("through the `tools` gateway (`action=list` / `action=enable`).\n\n")

	for _, c := range cats {
		entries := byCat[c]
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		fmt.Fprintf(&b, "## %s\n\n", c)
		b.WriteString("| Tool | Core | Summary |\n|------|------|---------|\n")
		for _, e := range entries {
			core := ""
			if e.Core {
				core = "✓"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", e.Name, core, e.Summary)
		}
		b.WriteString("\n")
	}

	if len(prompts) > 0 {
		sort.Slice(prompts, func(i, j int) bool { return prompts[i].Name < prompts[j].Name })
		b.WriteString("## prompts\n\n")
		b.WriteString("Guided multi-step workflows (MCP prompts).\n\n")
		b.WriteString("| Prompt | Summary |\n|--------|---------|\n")
		for _, e := range prompts {
			fmt.Fprintf(&b, "| `%s` | %s |\n", e.Name, e.Summary)
		}
		b.WriteString("\n")
	}

	out := os.Args[1]
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}
