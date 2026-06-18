// Command treeman-gen-config-docs renders a complete treeman config
// reference to `docs/config-reference.md` from the SAME reflected schema
// that produces `schemas/treeman.schema.json` (internal/schema).
//
// Every config field and its doc comment flows into the reference
// automatically, so adding a Go field with a `//` comment documents it
// in three places at once: editor hinting (JSON Schema), the reference
// page, and validation. Wired into `just sync-docs` so PRs catch drift.
//
// The page has three parts:
//   - Config layers: how the user-global `config.yaml` and per-repo
//     `.treeman.yaml` merge, plus the per-key scope table (which file a
//     key may appear in). Scopes come from config.FieldScopes().
//   - Generated YAML examples: one full example per layer (global +
//     repo), walked from the schema so they never drift.
//   - Field reference: top-level keys (tagged with scope) + every named
//     type with its fields.
package main

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/schema"
)

func main() {
	root := schema.Reflect()
	scopes := config.FieldScopes()

	var b strings.Builder
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("[← back to README](../README.md)\n\n")
	b.WriteString("Auto-generated from the `config.Config` Go types (the same\n")
	b.WriteString("reflection that produces `schemas/treeman.schema.json`).\n")
	b.WriteString("Run `just sync-docs` after touching a config field to refresh.\n")
	b.WriteString("For worked examples and guidance see [configuration.md](configuration.md).\n\n")

	writeLayers(&b, root, scopes)
	writeExamples(&b, root, scopes)
	writeFieldReference(&b, root, scopes)

	out := os.Args[1]
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}

// writeLayers documents the layered config model + the per-key scope
// table, driven by config.FieldScopes().
func writeLayers(b *strings.Builder, root *jsonschema.Schema, scopes map[string]string) {
	b.WriteString("## Config layers\n\n")
	b.WriteString("treeman reads config from two files that merge into one effective config:\n\n")
	b.WriteString(
		"1. **User-global** — `~/.config/treeman/config.yaml` (`$XDG_CONFIG_HOME/treeman/config.yaml`). Machine-wide defaults shared by every repo. Scaffold it with `treeman init --global`.\n",
	)
	b.WriteString(
		"2. **Per-repo** — `<repo>/.treeman.yaml` (plus an optional git-ignored `.treeman.local.yaml` overlay). Project-specific settings.\n\n",
	)
	b.WriteString(
		"Later layers override earlier ones. Each top-level key has a **scope** that determines which file it may appear in — a key in the wrong file is a hard error at load time (no flag relaxes it):\n\n",
	)
	b.WriteString("- **global** — only valid in the user-global config.\n")
	b.WriteString("- **repo** — only valid in a repo `.treeman.yaml`.\n")
	b.WriteString("- **both** — valid in either; the global value is the default, a repo value overrides it.\n\n")

	// Scope table in the schema's property order.
	b.WriteString("| Key | Scope |\n|-----|-------|\n")
	if root.Properties != nil {
		for pair := root.Properties.Oldest(); pair != nil; pair = pair.Next() {
			sc := scopes[pair.Key]
			if sc == "" {
				sc = "both"
			}
			fmt.Fprintf(b, "| `%s` | %s |\n", pair.Key, sc)
		}
	}
	b.WriteString("\n")
}

// writeExamples emits one generated YAML example per layer. The global
// example carries scope=global+both keys, the repo example scope=repo+
// both. Both are walked from the schema so they track the structs.
func writeExamples(b *strings.Builder, root *jsonschema.Schema, scopes map[string]string) {
	b.WriteString("## Generated examples\n\n")
	b.WriteString(
		"Complete examples covering every key valid in each layer, generated from the schema (placeholder values — replace with real ones).\n\n",
	)

	b.WriteString("### User-global `~/.config/treeman/config.yaml`\n\n")
	b.WriteString("```yaml\n")
	b.WriteString(exampleYAML(root, scopes, "global"))
	b.WriteString("```\n\n")

	b.WriteString("### Per-repo `.treeman.yaml`\n\n")
	b.WriteString("```yaml\n")
	b.WriteString(exampleYAML(root, scopes, "repo"))
	b.WriteString("```\n\n")
}

// exampleYAML builds a YAML document of every top-level key whose scope
// belongs in `layer` ("global" or "repo"; "both" keys appear in both),
// with placeholder values walked from the schema and the first line of
// each field's doc comment as a comment.
func exampleYAML(root *jsonschema.Schema, scopes map[string]string, layer string) string {
	doc := &yaml.Node{Kind: yaml.MappingNode}
	if root.Properties != nil {
		for pair := root.Properties.Oldest(); pair != nil; pair = pair.Next() {
			sc := scopes[pair.Key]
			if sc == "" {
				sc = "both"
			}
			if sc != "both" && sc != layer {
				continue
			}
			key := &yaml.Node{Kind: yaml.ScalarNode, Value: pair.Key}
			if c := firstLine(pair.Value.Description); c != "" {
				key.HeadComment = c
			}
			doc.Content = append(doc.Content, key, exampleNode(pair.Value, root, 0, map[string]int{}))
		}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "# (failed to render example)\n"
	}
	return string(out)
}

// exampleNode builds a placeholder *yaml.Node for one schema node,
// resolving $refs against root.Definitions. Recursion is bounded by
// depth and a per-type visit count so self-referential types (Action,
// DumpSpec) don't loop forever.
func exampleNode(s *jsonschema.Schema, root *jsonschema.Schema, depth int, seen map[string]int) *yaml.Node {
	if s == nil || depth > 8 {
		return scalar("...")
	}
	// oneOf/anyOf: document the first (simplest) branch.
	if len(s.OneOf) > 0 {
		return exampleNode(s.OneOf[0], root, depth, seen)
	}
	if len(s.AnyOf) > 0 {
		return exampleNode(s.AnyOf[0], root, depth, seen)
	}
	if s.Ref != "" {
		name := s.Ref[strings.LastIndex(s.Ref, "/")+1:]
		if seen[name] >= 1 {
			return scalar("...") // cycle guard
		}
		if def, ok := root.Definitions[name]; ok {
			seen[name]++
			n := exampleNode(def, root, depth+1, seen)
			seen[name]--
			return n
		}
		return scalar("...")
	}
	if len(s.Enum) > 0 {
		return scalar(fmt.Sprintf("%v", s.Enum[0]))
	}
	switch s.Type {
	case "array":
		return &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{
			exampleNode(s.Items, root, depth+1, seen),
		}}
	case "object", "":
		m := &yaml.Node{Kind: yaml.MappingNode}
		if s.Properties != nil && s.Properties.Len() > 0 {
			for pair := s.Properties.Oldest(); pair != nil; pair = pair.Next() {
				key := &yaml.Node{Kind: yaml.ScalarNode, Value: pair.Key}
				m.Content = append(m.Content, key, exampleNode(pair.Value, root, depth+1, seen))
			}
			return m
		}
		// Map type (additionalProperties is a schema): show one entry.
		if s.AdditionalProperties != nil && (s.AdditionalProperties.Ref != "" || s.AdditionalProperties.Type != "") {
			m.Content = append(m.Content,
				scalar("<name>"),
				exampleNode(s.AdditionalProperties, root, depth+1, seen))
			return m
		}
		// Empty object — flow style so it renders as `{}`.
		m.Style = yaml.FlowStyle
		return m
	case "boolean":
		return scalar("false")
	case "integer", "number":
		return scalar("0")
	default:
		return scalar("...")
	}
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

// firstLine returns the first non-empty line of a doc comment, trimmed,
// for use as a one-line YAML comment.
func firstLine(s string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// writeFieldReference renders the top-level keys (annotated with scope)
// and every named type.
func writeFieldReference(b *strings.Builder, root *jsonschema.Schema, scopes map[string]string) {
	b.WriteString("## Top-level keys\n\n")
	if root.Properties != nil {
		for pair := root.Properties.Oldest(); pair != nil; pair = pair.Next() {
			sc := scopes[pair.Key]
			if sc == "" {
				sc = "both"
			}
			writeField(b, "###", pair.Key, pair.Value, required(root, pair.Key), sc)
		}
	}

	if len(root.Definitions) > 0 {
		b.WriteString("## Types\n\n")
		names := make([]string, 0, len(root.Definitions))
		for name := range root.Definitions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			def := root.Definitions[name]
			fmt.Fprintf(b, "### %s\n\n", name)
			if d := strings.TrimSpace(def.Description); d != "" {
				fmt.Fprintf(b, "%s\n\n", d)
			}
			if def.Properties != nil {
				for pair := def.Properties.Oldest(); pair != nil; pair = pair.Next() {
					writeField(b, "####", pair.Key, pair.Value, required(def, pair.Key), "")
				}
			}
		}
	}
}

// writeField renders one property: `key` *(type)* [scope] [required],
// then the field's description as a paragraph (the docstring's own
// newlines and bullet lists carry through as markdown). An empty scope
// suppresses the scope tag (used for nested type fields, which inherit
// their parent's scope).
func writeField(b *strings.Builder, heading, key string, s *jsonschema.Schema, req bool, scope string) {
	tags := ""
	if scope != "" {
		tags += fmt.Sprintf(" _[%s]_", scope)
	}
	if req {
		tags += " — **required**"
	}
	fmt.Fprintf(b, "%s `%s` *(%s)*%s\n\n", heading, key, typeLabel(s), tags)
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
	return slices.Contains(s.Required, key)
}
