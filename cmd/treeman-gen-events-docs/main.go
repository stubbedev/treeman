// Command treeman-gen-events-docs renders the event-type reference to
// the file named in os.Args[1] by AST-parsing internal/store/
// eventtypes.go: the `// group` comments become section headings and
// each Evt* constant becomes a row (its wire value + Go name). Adding a
// constant documents it automatically. Wired into `just sync-docs`.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

type (
	entry struct{ name, value string }
	group struct {
		title   string
		entries []entry
	}
)

func main() {
	src := "internal/store/eventtypes.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}

	var groups []group
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		cur := &group{title: "General"}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 || len(vs.Values) == 0 {
				continue
			}
			// A doc comment immediately above a const starts a new group.
			if vs.Doc != nil {
				title := strings.TrimSpace(vs.Doc.Text())
				title = strings.SplitN(title, "\n", 2)[0]
				title = strings.TrimRight(title, ".")
				if cur != nil && len(cur.entries) > 0 {
					groups = append(groups, *cur)
				}
				cur = &group{title: title}
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			cur.entries = append(cur.entries, entry{
				name:  vs.Names[0].Name,
				value: strings.Trim(lit.Value, `"`),
			})
		}
		if cur != nil && len(cur.entries) > 0 {
			groups = append(groups, *cur)
		}
	}

	var b strings.Builder
	b.WriteString("# Event reference\n\n")
	b.WriteString("[← back to README](../README.md)\n\n")
	b.WriteString("Auto-generated from `internal/store/eventtypes.go` (the `store.Evt*`\n")
	b.WriteString("constants — the single source of truth for every `event_type` written\n")
	b.WriteString("to the SQLite log). Run `just sync-docs` after adding an event to refresh.\n\n")
	b.WriteString("Filter the log on these with `treeman logs tail --event-type <value>` or\n")
	b.WriteString("the `logs_query` MCP tool. Naming scheme: `domain:object:stage` — the\n")
	b.WriteString("`domain` is the config option the event relates to or the subsystem\n")
	b.WriteString("that emits it.\n\n")

	for _, g := range groups {
		fmt.Fprintf(&b, "## %s\n\n", g.title)
		b.WriteString("| Event | Constant |\n|-------|----------|\n")
		for _, e := range g.entries {
			fmt.Fprintf(&b, "| `%s` | `store.%s` |\n", e.value, e.name)
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
