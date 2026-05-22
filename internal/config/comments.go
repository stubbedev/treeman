// Package config — comment harvester for JSON Schema hover docs.
//
// invopop/jsonschema's AddGoComments walks the filesystem to harvest
// Go doc comments, which doesn't work once treeman is installed as a
// binary (no source on disk). We embed config.go at build time and
// AST-parse it on demand so the comments ship with the binary.
package config

import (
	_ "embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

//go:embed config.go
var configSource []byte

// commentPkgPath must match reflect.Type.PkgPath() for the types in
// this file. invopop builds its CommentMap key as
// `<PkgPath>.<TypeName>[.<FieldName>]`.
const commentPkgPath = "github.com/stubbedev/treeman/internal/config"

// CommentMap returns the invopop CommentMap built from the embedded
// config.go source. Keys are `<pkg>.<Type>` and `<pkg>.<Type>.<Field>`;
// values are the trimmed doc comment text.
//
// Type-level comments are kept in full (no Synopsis truncation) so
// hover tooltips include the rationale, not just the first sentence.
func CommentMap() map[string]string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "config.go", configSource, parser.ParseComments)
	if err != nil {
		return nil
	}
	m := map[string]string{}
	var pendingGenDoc string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GenDecl:
			pendingGenDoc = x.Doc.Text()
		case *ast.TypeSpec:
			typ := x.Name.Name
			if !ast.IsExported(typ) {
				return true
			}
			txt := x.Doc.Text()
			if txt == "" {
				txt = pendingGenDoc
				pendingGenDoc = ""
			}
			if txt != "" {
				m[fmt.Sprintf("%s.%s", commentPkgPath, typ)] = strings.TrimSpace(txt)
			}
			st, ok := x.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, field := range st.Fields.List {
				ftxt := field.Doc.Text()
				if ftxt == "" {
					ftxt = field.Comment.Text()
				}
				if ftxt == "" {
					continue
				}
				ftxt = strings.TrimSpace(ftxt)
				for _, name := range field.Names {
					if !ast.IsExported(name.Name) {
						continue
					}
					m[fmt.Sprintf("%s.%s.%s", commentPkgPath, typ, name.Name)] = ftxt
				}
			}
			return false
		}
		return true
	})
	return m
}
