// Package patcher rewrites individual files inside a worktree with
// per-worktree values (slug-substituted DB names, cache prefixes,
// etc.). Six drivers are supported:
//
//   - dotenv  : `KEY=value` lines
//   - phpunit : `<env name="KEY" value="..."/>` inside `<php>`
//   - yaml    : dotted-path key set, preserving comments
//   - json    : dotted-path key set on a JSON document
//   - toml    : dotted-path key set on a TOML document
//   - ini     : `section.key` set on an INI file
//
// Each rewritten file is wired through git's clean/smudge filter
// (EnsureFilter) so `git pull` / `git checkout` can overwrite
// patched content without the "would be overwritten by merge"
// refusal. Smudge re-applies the patch on the way back to the
// working tree, so per-worktree values survive the pull. See
// filter.go for the clean/smudge semantics and install.go for the
// git config + info/attributes wiring.
package patcher

import (
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// xmlAttrEscape returns `value` escaped for use inside an XML
// attribute value. encoding/xml exposes the canonical escaper via
// xml.EscapeText, which writes both `&` and `<`/`>`/`"` correctly.
func xmlAttrEscape(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

// Outcome reports what the patch call did.
type Outcome int

const (
	// Updated — file content changed and was written.
	Updated Outcome = iota
	// Unchanged — file already had the requested values.
	Unchanged
	// Missing — file does not exist; nothing written.
	Missing
)

// Pair is a (key, value) tuple. Slices of these drive the dotenv and
// phpunit_env patchers.
type Pair struct {
	Key   string
	Value string
}

// PatchEnvFile rewrites a dotenv-style file at `path`. Existing
// `KEY=...` lines are replaced in place; missing keys appended at
// the end with a trailing newline.
func PatchEnvFile(path string, pairs []Pair) (Outcome, error) {
	original, err := readFile(path)
	if err != nil {
		return Missing, nil
	}
	content := original
	for _, p := range pairs {
		content = patchEnvOne(content, p.Key, p.Value)
	}
	if content == original {
		return Unchanged, nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Updated, fmt.Errorf("write %s: %w", path, err)
	}
	return Updated, nil
}

func patchEnvOne(content, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `\s*=.*$`)
	// regexp's Replace expands `$1`, `${name}`, `$$`, etc. in the
	// replacement string. Use ReplaceAllLiteralString so a slug or
	// value containing `$` doesn't accidentally pull in match groups.
	if re.MatchString(content) {
		return re.ReplaceAllLiteralString(content, key+"="+value)
	}
	out := content
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += key + "=" + value + "\n"
	return out
}

// PatchPhpunitFile rewrites a phpunit.xml file at `path`. Existing
// `<env name="KEY" .../>` entries are replaced in place; missing
// entries inserted before `</php>` with the same `\t...\n\t</php>`
// indent the bash hook produced (preserves diff parity).
func PatchPhpunitFile(path string, pairs []Pair) (Outcome, error) {
	original, err := readFile(path)
	if err != nil {
		return Missing, nil
	}
	content := original
	for _, p := range pairs {
		content = patchPhpunitOne(content, p.Key, p.Value)
	}
	if content == original {
		return Unchanged, nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Updated, fmt.Errorf("write %s: %w", path, err)
	}
	return Updated, nil
}

func patchPhpunitOne(content, key, value string) string {
	re := regexp.MustCompile(`<env name="` + regexp.QuoteMeta(key) + `"[^/]*/>`)
	replacement := fmt.Sprintf(`<env name="%s" value="%s" force="true"/>`,
		xmlAttrEscape(key), xmlAttrEscape(value))
	if re.MatchString(content) {
		return re.ReplaceAllLiteralString(content, replacement)
	}
	inject := "\t" + replacement + "\n\t</php>"
	return strings.Replace(content, "</php>", inject, 1)
}

// PatchYAMLFile sets each (dotted-path, value) pair inside the YAML
// document at `path`, preserving comments and key ordering. New
// intermediate keys are created; existing-key updates replace the
// value node only. Reuses internal/yamlpatch, which is the same
// engine that drives `treeman config set`.
func PatchYAMLFile(path string, pairs map[string]string) (Outcome, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Missing, nil
		}
		return Missing, fmt.Errorf("read %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return Missing, fmt.Errorf("parse %s: %w", path, err)
	}
	changed := false
	for _, k := range sortedKeys(pairs) {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return Missing, fmt.Errorf("yaml driver: path %q: %w", k, err)
		}
		newNode, err := yamlpatch.ValueToNode(pairs[k])
		if err != nil {
			return Missing, fmt.Errorf("yaml driver: value for %q: %w", k, err)
		}
		prev, err := yamlpatch.Set(&doc, segs, newNode)
		if err != nil {
			return Missing, fmt.Errorf("yaml driver: set %q: %w", k, err)
		}
		if prev == nil || prev.Value != newNode.Value {
			changed = true
		}
	}
	if !changed {
		return Unchanged, nil
	}
	out, err := yamlpatch.Marshal(&doc)
	if err != nil {
		return Updated, fmt.Errorf("yaml driver: marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return Updated, fmt.Errorf("write %s: %w", path, err)
	}
	return Updated, nil
}

// PatchJSONFile sets each (dotted-path, value) pair inside the JSON
// document at `path`. Path syntax matches yamlpatch (`a.b[0].c`).
// Integer-shaped strings are written as JSON numbers; `true`/`false`
// stay strings (the user has to disambiguate by editing JSON
// directly if they need a real bool — `treeman init` only ever
// writes strings into json patches, so this constraint is fine for
// the common case).
func PatchJSONFile(path string, pairs map[string]string) (Outcome, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Missing, nil
		}
		return Missing, fmt.Errorf("read %s: %w", path, err)
	}
	// Byte-preserving value splice for existing keys (create falls back
	// to the reordering marshal). Keeps clean(apply(HEAD)) == HEAD so the
	// patched file never shows as modified in `git status`.
	out, changed, err := setJSONInPlace(string(body), pairs)
	if err != nil {
		return Missing, fmt.Errorf("json driver %s: %w", path, err)
	}
	if !changed {
		return Unchanged, nil
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return Updated, fmt.Errorf("write %s: %w", path, err)
	}
	return Updated, nil
}

// jsonScalar coerces a template-rendered string into the most
// natural JSON scalar. Pure-digit strings become integers so e.g.
// `redis.db: "7"` lands as a JSON number; everything else stays a
// string. Booleans / nulls intentionally stay strings — too easy to
// surprise the user by turning a literal "true" into the bool.
func jsonScalar(v string) any {
	if v == "" {
		return v
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return v
}

// setJSONPath walks the decoded JSON tree along `segs` and replaces
// the terminal node. Mirrors yamlpatch.Set for JSON. Maps are walked
// via map[string]any; sequences via []any. Missing intermediate keys
// (in maps) are created; out-of-range sequence indices error so the
// caller can't silently reshape a list. Returns the previous value
// (or nil when a new key was created).
//
// Implementation: walk to the parent of the terminal segment, then
// mutate parent[terminal] = newVal. Both maps and slices are
// reference types in Go, so mutating through `parent` propagates
// back to root without needing addressable values.
func setJSONPath(root any, segs []yamlpatch.Segment, newVal any) (any, error) {
	if len(segs) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	parent := root
	// Walk to parent of last segment, creating missing map intermediates.
	for i := 0; i < len(segs)-1; i++ {
		seg := segs[i]
		if seg.IsIndex {
			arr, ok := parent.([]any)
			if !ok {
				return nil, fmt.Errorf("segment %d: expected array, got %T", i, parent)
			}
			if seg.Idx < 0 || seg.Idx >= len(arr) {
				return nil, fmt.Errorf("segment %d: index %d out of range (len=%d)", i, seg.Idx, len(arr))
			}
			parent = arr[seg.Idx]
			continue
		}
		m, ok := parent.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("segment %d (%q): expected object, got %T", i, seg.Key, parent)
		}
		next, exists := m[seg.Key]
		if !exists {
			next = map[string]any{}
			m[seg.Key] = next
		}
		parent = next
	}
	last := segs[len(segs)-1]
	if last.IsIndex {
		arr, ok := parent.([]any)
		if !ok {
			return nil, fmt.Errorf("terminal segment: expected array, got %T", parent)
		}
		if last.Idx < 0 || last.Idx >= len(arr) {
			return nil, fmt.Errorf("terminal segment: index %d out of range (len=%d)", last.Idx, len(arr))
		}
		prev := arr[last.Idx]
		arr[last.Idx] = newVal
		return prev, nil
	}
	m, ok := parent.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("terminal segment (%q): expected object, got %T", last.Key, parent)
	}
	prev := m[last.Key]
	m[last.Key] = newVal
	return prev, nil
}

func jsonEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
