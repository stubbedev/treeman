// Command treeman-gen-rpc-docs renders the RPC surface reference to the
// file named in os.Args[1] by AST-parsing internal/rpc/rpc.go: the
// Method*, Kind*, Task*, and Param* constant blocks become tables, each
// row carrying the const's trailing `//` comment as its description.
// Wired into `just sync-docs`.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

type row struct{ value, name, doc string }

// section maps a const-name prefix to a table heading + blurb.
type section struct {
	prefix, title, blurb string
	rows                 []row
}

func main() {
	src := "internal/rpc/rpc.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		os.Exit(1)
	}

	secs := []*section{
		{
			prefix: "Method",
			title:  "Methods",
			blurb:  "RPC methods the daemon answers. Wire envelope: `{\"method\": <m>, \"<m>\": {…args}}` (protocol v2).",
		},
		{prefix: "Kind", title: "Response kinds", blurb: "The `kind` field on every response."},
		{prefix: "Task", title: "Plan tasks", blurb: "State mutations the daemon performs; submitted as a plan via the `run_plan` method."},
		{prefix: "Param", title: "Task params", blurb: "String-keyed side-channel on a Task (booleans encoded as `\"1\"`)."},
	}

	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 || len(vs.Values) == 0 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			name := vs.Names[0].Name
			for _, s := range secs {
				if strings.HasPrefix(name, s.prefix) {
					doc := strings.TrimSpace(vs.Comment.Text())
					s.rows = append(s.rows, row{
						value: strings.Trim(lit.Value, `"`),
						name:  name,
						doc:   doc,
					})
					break
				}
			}
		}
	}

	var b strings.Builder
	b.WriteString("# RPC reference\n\n")
	b.WriteString("[← back to docs](README.md)\n\n")
	b.WriteString("Auto-generated from the `Method*` / `Kind*` / `Task*` / `Param*`\n")
	b.WriteString("constants in `internal/rpc/rpc.go`. Run `just sync-docs` after touching\n")
	b.WriteString("the RPC surface to refresh.\n\n")
	for _, s := range secs {
		if len(s.rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", s.title, s.blurb)
		b.WriteString("| Value | Constant | Notes |\n|-------|----------|-------|\n")
		for _, r := range s.rows {
			fmt.Fprintf(&b, "| `%s` | `rpc.%s` | %s |\n", r.value, r.name, r.doc)
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
