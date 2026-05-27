// Command treeman-gen-config-docs renders a complete `.treeman.yaml`
// field reference to `docs/config-reference.md` from the SAME reflected
// schema that produces `schemas/treeman.schema.json` (internal/schema).
//
// Every config field and its doc comment flows into the reference
// automatically, so adding a Go field with a `//` comment documents it
// in three places at once: editor hinting (JSON Schema), the reference
// page, and validation. Wired into `just sync-docs` so PRs catch drift.
//
// The renderer is intentionally shallow: top-level keys link into a
// per-type section, and each type lists its fields with resolved types
// and descriptions. Deep inlining would just duplicate the type
// sections.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"

	"github.com/stubbedev/treeman/internal/schema"
)

func main() {
	root := schema.Reflect()

	var b strings.Builder
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("[← back to README](../README.md)\n\n")
	b.WriteString("Auto-generated from the `config.Config` Go types (the same\n")
	b.WriteString("reflection that produces `schemas/treeman.schema.json`).\n")
	b.WriteString("Run `just sync-docs` after touching a config field to refresh.\n")
	b.WriteString("For worked examples and guidance see [configuration.md](configuration.md).\n\n")

	// Top-level keys.
	b.WriteString("## Top-level keys\n\n")
	if root.Properties != nil {
		for pair := root.Properties.Oldest(); pair != nil; pair = pair.Next() {
			writeField(&b, "###", pair.Key, pair.Value, required(root, pair.Key))
		}
	}

	// Named types (`$defs`), sorted for stable output.
	if len(root.Definitions) > 0 {
		b.WriteString("## Types\n\n")
		names := make([]string, 0, len(root.Definitions))
		for name := range root.Definitions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			def := root.Definitions[name]
			fmt.Fprintf(&b, "### %s\n\n", name)
			if d := strings.TrimSpace(def.Description); d != "" {
				fmt.Fprintf(&b, "%s\n\n", d)
			}
			if def.Properties != nil {
				for pair := def.Properties.Oldest(); pair != nil; pair = pair.Next() {
					writeField(&b, "####", pair.Key, pair.Value, required(def, pair.Key))
				}
			}
		}
	}

	out := os.Args[1]
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}

// writeField renders one property: `key` *(type)* [required], then the
// field's description as a paragraph (the docstring's own newlines and
// bullet lists carry through as markdown).
func writeField(b *strings.Builder, heading, key string, s *jsonschema.Schema, req bool) {
	reqTag := ""
	if req {
		reqTag = " — **required**"
	}
	fmt.Fprintf(b, "%s `%s` *(%s)*%s\n\n", heading, key, typeLabel(s), reqTag)
	if d := strings.TrimSpace(s.Description); d != "" {
		fmt.Fprintf(b, "%s\n\n", d)
	}
	if extra := constraints(s); extra != "" {
		fmt.Fprintf(b, "%s\n\n", extra)
	}
}

// typeLabel resolves a schema node to a short human type string,
// linking named types to their section.
func typeLabel(s *jsonschema.Schema) string {
	switch {
	case s == nil:
		return "any"
	case s.Ref != "":
		return defLink(s.Ref)
	case s.Type == "array" && s.Items != nil:
		return "array of " + typeLabel(s.Items)
	case s.Type == "object" && s.AdditionalProperties != nil && s.AdditionalProperties.Ref != "":
		return "map of name → " + defLink(s.AdditionalProperties.Ref)
	case len(s.OneOf) > 0:
		return "one of: " + joinLabels(s.OneOf)
	case len(s.AnyOf) > 0:
		return "one of: " + joinLabels(s.AnyOf)
	case s.Type != "":
		return s.Type
	default:
		return "object"
	}
}

func joinLabels(schemas []*jsonschema.Schema) string {
	parts := make([]string, 0, len(schemas))
	for _, s := range schemas {
		parts = append(parts, typeLabel(s))
	}
	return strings.Join(parts, ", ")
}

// defLink turns a `#/$defs/DatabaseConfig` ref into a markdown link to
// the matching `### DatabaseConfig` heading.
func defLink(ref string) string {
	name := ref[strings.LastIndex(ref, "/")+1:]
	return fmt.Sprintf("[%s](#%s)", name, strings.ToLower(name))
}

// constraints renders enum / min / max hints when present.
func constraints(s *jsonschema.Schema) string {
	var parts []string
	if len(s.Enum) > 0 {
		vals := make([]string, 0, len(s.Enum))
		for _, v := range s.Enum {
			vals = append(vals, fmt.Sprintf("`%v`", v))
		}
		parts = append(parts, "Allowed: "+strings.Join(vals, ", "))
	}
	if s.Minimum != "" {
		parts = append(parts, "min: "+string(s.Minimum))
	}
	if s.Maximum != "" {
		parts = append(parts, "max: "+string(s.Maximum))
	}
	if len(parts) == 0 {
		return ""
	}
	return "_" + strings.Join(parts, " · ") + "_"
}

// required reports whether `key` is in the schema's Required list.
func required(s *jsonschema.Schema, key string) bool {
	for _, r := range s.Required {
		if r == key {
			return true
		}
	}
	return false
}
