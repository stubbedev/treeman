// Filter is the git clean/smudge implementation for `patches:`. It
// replaces the v2.4 `--skip-worktree` mechanism so that `git pull` /
// `git checkout` can overwrite a patched file without the
// "Your local changes would be overwritten by merge" refusal —
// possible only because git's safety check runs the WORKING TREE
// through `clean` before comparing to the index, and our clean
// rewrites the patched keys back to whatever HEAD has for the file.
//
// Smudge is git's inverse direction (index → working tree): take the
// canonical content and overlay the per-worktree values, producing
// exactly what `patcher.Apply` would have written under the old
// model.
//
// Why consulting HEAD on clean: the alternative is stripping patched
// keys outright. That would silently rewrite the committed form on
// the next `git commit -a`, which violates the "treeman owns these
// keys but the canonical content is the user's" contract. By
// projecting HEAD's value back through clean, the index/diff/status
// pipeline sees the file as unchanged from HEAD for the patched
// keys, leaving the rest of the user's edits intact.
//
// All six drivers (dotenv/phpunit/yaml/json/toml/ini) share the
// driver dispatch path; each provides `applyContent` (in-memory
// patch — used by smudge) and `extractContent` (in-memory read —
// used by clean to pull HEAD's values).
package patcher

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/ini.v1"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/template"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// Smudge applies one Patch's `set:` to `content` in memory. Mirrors
// `Apply`'s patch-write step exactly — same driver dispatch, same
// renderTemplates flow — but returns a string instead of writing
// to disk. Git invokes this via `filter.treeman-patch.smudge` when
// populating the working tree from the index.
func Smudge(p config.Patch, content string, tplCtx template.Context) (string, error) {
	driver, err := patchDriver(p)
	if err != nil {
		return "", err
	}
	rendered, err := renderTemplates(p.File, p.Set, tplCtx)
	if err != nil {
		return "", err
	}
	return applyContent(driver, content, rendered)
}

// Clean projects each `set:` key back to its HEAD form so the
// working tree compares equal to the index/HEAD for patched keys.
// Other keys in `content` pass through unchanged so user edits to
// non-patched parts of the file still surface in `git status` /
// `git diff`.
//
// Per-key behaviour:
//
//   - key defined in HEAD → restore HEAD's value (project canonical
//     content back so git sees the file unchanged for that key);
//   - key NOT in HEAD     → strip the key from `content` (Apply
//     APPENDED it because HEAD doesn't have it; if we leave it in
//     the clean output, `git add --renormalize` stages it and the
//     file is dirty forever).
//
// `headContent` empty (file not in HEAD) bypasses clean entirely:
// there's nothing to project from, and on a never-committed file
// stripping every patched key would zero useful content.
func Clean(p config.Patch, content, headContent string) (string, error) {
	driver, err := patchDriver(p)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(headContent) == "" {
		return content, nil
	}
	keys := setKeys(p.Set)
	headVals, err := extractContent(driver, headContent, keys)
	if err != nil {
		return "", err
	}
	// Two-phase clean:
	//
	//   1. Drop keys that `patches[].set` owns but HEAD does NOT
	//      define. These were APPENDED by Apply (dotenv adds the line,
	//      phpunit injects `<env .../>` before `</php>`, yaml/json/
	//      toml/ini create the path). Leaving them in clean's output
	//      means `git add --renormalize` stages them and the file
	//      stays dirty forever — the bug that caused KON-12446 /
	//      KON-12445 `.env.testing` / `phpunit.xml` to show as
	//      "modified" in `git status` even on a fresh worktree.
	//   2. Restore HEAD's value for keys HEAD does define. Same as
	//      before — projects the canonical value back so git sees the
	//      file as unchanged for those keys.
	absent := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := headVals[k]; !ok {
			absent = append(absent, k)
		}
	}
	if len(absent) > 0 {
		content, err = removeContent(driver, content, absent)
		if err != nil {
			return "", err
		}
	}
	if len(headVals) == 0 {
		return content, nil
	}
	return applyContent(driver, content, headVals)
}

// patchDriver collapses the explicit `format:` + extension-based
// auto-detect into a single canonical driver name. Mirrors the
// switch in Apply so the filter and the legacy file-write path
// pick the same driver for the same input.
func patchDriver(p config.Patch) (string, error) {
	d := strings.ToLower(strings.TrimSpace(p.Format))
	if d == "" {
		d = detectFormat(p.File)
	}
	if d == "" {
		return "", fmt.Errorf("patch %s: cannot infer format from extension; set `format:` explicitly", p.File)
	}
	// phpunit_env is a legacy alias for phpunit.
	if d == "phpunit_env" {
		d = "phpunit"
	}
	return d, nil
}

func setKeys(set map[string]string) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// applyContent is the in-memory equivalent of Patch<Driver>File for
// each supported driver. `pairs` keys are the patched paths (dotted
// for yaml/json/toml/ini), values are the rendered strings — same
// shape Apply already builds.
func applyContent(driver, content string, pairs map[string]string) (string, error) {
	if len(pairs) == 0 {
		return content, nil
	}
	switch driver {
	case "dotenv":
		return applyDotenvContent(content, pairs), nil
	case "phpunit":
		return applyPhpunitContent(content, pairs), nil
	case "yaml":
		return applyYAMLContent(content, pairs)
	case "json":
		return applyJSONContent(content, pairs)
	case "toml":
		return applyTOMLContent(content, pairs)
	case "ini":
		return applyINIContent(content, pairs)
	}
	return "", fmt.Errorf("unknown driver %q", driver)
}

// extractContent reads `content` with the driver's parser and
// returns the values for `keys`. Missing keys are omitted from the
// returned map; the caller treats that as "leave the working-tree
// value alone for this key".
func extractContent(driver, content string, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	switch driver {
	case "dotenv":
		return extractDotenv(content, keys), nil
	case "phpunit":
		return extractPhpunit(content, keys), nil
	case "yaml":
		return extractYAML(content, keys)
	case "json":
		return extractJSON(content, keys)
	case "toml":
		return extractTOML(content, keys)
	case "ini":
		return extractINI(content, keys)
	}
	return nil, fmt.Errorf("unknown driver %q", driver)
}

// removeContent strips `keys` from `content` using the driver's
// parser. Used by Clean to undo Apply's "append if missing" behaviour
// for any key in `patches[].set` that HEAD doesn't define — without
// this step the appended lines persist in the index after
// `git add --renormalize` and the file shows as permanently modified.
//
// Removal is a deliberate write: treeman owns the keys in
// `patches[].set` end-to-end, so stripping them on clean is symmetric
// with Apply having created them on smudge. Keys that aren't present
// in `content` to begin with are silently skipped.
func removeContent(driver, content string, keys []string) (string, error) {
	if len(keys) == 0 {
		return content, nil
	}
	switch driver {
	case "dotenv":
		return removeDotenvContent(content, keys), nil
	case "phpunit":
		return removePhpunitContent(content, keys), nil
	case "yaml":
		return removeYAMLContent(content, keys)
	case "json":
		return removeJSONContent(content, keys)
	case "toml":
		return removeTOMLContent(content, keys)
	case "ini":
		return removeINIContent(content, keys)
	}
	return "", fmt.Errorf("unknown driver %q", driver)
}

// ─── dotenv ──────────────────────────────────────────────────────────

func applyDotenvContent(content string, pairs map[string]string) string {
	for _, k := range sortedKeys(pairs) {
		content = patchEnvOne(content, k, pairs[k])
	}
	return content
}

func extractDotenv(content string, keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(k) + `\s*=(.*)$`)
		m := re.FindStringSubmatch(content)
		if len(m) == 2 {
			out[k] = m[1]
		}
	}
	return out
}

// ─── phpunit ─────────────────────────────────────────────────────────

func applyPhpunitContent(content string, pairs map[string]string) string {
	for _, k := range sortedKeys(pairs) {
		content = patchPhpunitOne(content, k, pairs[k])
	}
	return content
}

func extractPhpunit(content string, keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		re := regexp.MustCompile(`<env name="` + regexp.QuoteMeta(k) + `"\s+value="([^"]*)"`)
		m := re.FindStringSubmatch(content)
		if len(m) == 2 {
			// Round-trip XML attribute escaping so the value we hand
			// back to applyContent comes out matching the canonical
			// committed form after re-escape.
			out[k] = xmlAttrUnescape(m[1])
		}
	}
	return out
}

// xmlAttrUnescape reverses xmlAttrEscape: turns &amp;/&lt;/&gt;/&quot;
// back into their literal forms. We use encoding/xml.Decoder to do
// the heavy lifting so it stays in lockstep with the escape direction.
func xmlAttrUnescape(s string) string {
	dec := xml.NewDecoder(strings.NewReader("<x a=\"" + s + "\"/>"))
	for {
		tok, err := dec.Token()
		if err != nil {
			return s
		}
		if se, ok := tok.(xml.StartElement); ok {
			if len(se.Attr) > 0 {
				return se.Attr[0].Value
			}
			return s
		}
	}
}

// ─── yaml ────────────────────────────────────────────────────────────

func applyYAMLContent(content string, pairs map[string]string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("yaml driver: parse: %w", err)
	}
	for _, k := range sortedKeys(pairs) {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return "", fmt.Errorf("yaml driver: path %q: %w", k, err)
		}
		newNode, err := yamlpatch.ValueToNode(pairs[k])
		if err != nil {
			return "", fmt.Errorf("yaml driver: value for %q: %w", k, err)
		}
		if _, err := yamlpatch.Set(&doc, segs, newNode); err != nil {
			return "", fmt.Errorf("yaml driver: set %q: %w", k, err)
		}
	}
	body, err := yamlpatch.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("yaml driver: marshal: %w", err)
	}
	return string(body), nil
}

func extractYAML(content string, keys []string) (map[string]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("yaml driver: parse: %w", err)
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return nil, fmt.Errorf("yaml driver: path %q: %w", k, err)
		}
		if v, ok := yamlScalarAt(&doc, segs); ok {
			out[k] = v
		}
	}
	return out, nil
}

// yamlScalarAt walks `segs` from the document root and returns the
// scalar value if the path terminates at a yaml.ScalarNode. Returns
// ("", false) for missing paths or non-scalar terminals — the clean
// caller treats both as "no canonical override available".
func yamlScalarAt(doc *yaml.Node, segs []yamlpatch.Segment) (string, bool) {
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	for _, seg := range segs {
		switch {
		case seg.IsIndex:
			if n.Kind != yaml.SequenceNode || seg.Idx >= len(n.Content) || seg.Idx < 0 {
				return "", false
			}
			n = n.Content[seg.Idx]
		default:
			if n.Kind != yaml.MappingNode {
				return "", false
			}
			found := false
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == seg.Key {
					n = n.Content[i+1]
					found = true
					break
				}
			}
			if !found {
				return "", false
			}
		}
	}
	if n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
}

// ─── json ────────────────────────────────────────────────────────────

func applyJSONContent(content string, pairs map[string]string) (string, error) {
	var doc any
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("json driver: parse: %w", err)
	}
	root := doc
	for _, k := range sortedKeys(pairs) {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return "", fmt.Errorf("json driver: path %q: %w", k, err)
		}
		newVal := jsonScalar(pairs[k])
		if _, err := setJSONPath(root, segs, newVal); err != nil {
			return "", fmt.Errorf("json driver: set %q: %w", k, err)
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("json driver: marshal: %w", err)
	}
	return string(out) + "\n", nil
}

func extractJSON(content string, keys []string) (map[string]string, error) {
	var doc any
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("json driver: parse: %w", err)
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return nil, fmt.Errorf("json driver: path %q: %w", k, err)
		}
		if v, ok := scalarAtJSONPath(doc, segs); ok {
			out[k] = v
		}
	}
	return out, nil
}

// scalarAtJSONPath walks `segs` from `root` and returns the scalar
// rendered as a string. Numbers come back via fmt.Sprintf("%v") so
// jsonScalar's "purely numeric strings → int64" rule round-trips
// cleanly (apply writes 5432 → extract reads "5432" → apply writes
// 5432 again).
func scalarAtJSONPath(root any, segs []yamlpatch.Segment) (string, bool) {
	cur := root
	for _, seg := range segs {
		switch v := cur.(type) {
		case map[string]any:
			if seg.IsIndex {
				return "", false
			}
			x, ok := v[seg.Key]
			if !ok {
				return "", false
			}
			cur = x
		case []any:
			if !seg.IsIndex || seg.Idx < 0 || seg.Idx >= len(v) {
				return "", false
			}
			cur = v[seg.Idx]
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case nil:
		return "", true
	case bool, float64, int, int64, json.Number:
		return fmt.Sprintf("%v", v), true
	}
	return "", false
}

// ─── toml ────────────────────────────────────────────────────────────

func applyTOMLContent(content string, pairs map[string]string) (string, error) {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("toml driver: parse: %w", err)
	}
	root := any(doc)
	for _, k := range sortedKeys(pairs) {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return "", fmt.Errorf("toml driver: path %q: %w", k, err)
		}
		if _, err := setJSONPath(root, segs, tomlScalar(pairs[k])); err != nil {
			return "", fmt.Errorf("toml driver: set %q: %w", k, err)
		}
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(doc); err != nil {
		return "", fmt.Errorf("toml driver: marshal: %w", err)
	}
	return buf.String(), nil
}

func extractTOML(content string, keys []string) (map[string]string, error) {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("toml driver: parse: %w", err)
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return nil, fmt.Errorf("toml driver: path %q: %w", k, err)
		}
		if v, ok := scalarAtJSONPath(any(doc), segs); ok {
			out[k] = v
		}
	}
	return out, nil
}

// ─── ini ─────────────────────────────────────────────────────────────

func applyINIContent(content string, pairs map[string]string) (string, error) {
	cfg, err := ini.Load([]byte(content))
	if err != nil {
		return "", fmt.Errorf("ini driver: parse: %w", err)
	}
	for _, k := range sortedKeys(pairs) {
		section, key, err := splitINIPath(k)
		if err != nil {
			return "", fmt.Errorf("ini driver: %w", err)
		}
		s, err := cfg.GetSection(section)
		if err != nil {
			s, _ = cfg.NewSection(section)
		}
		if s.HasKey(key) {
			s.Key(key).SetValue(pairs[k])
		} else {
			_, _ = s.NewKey(key, pairs[k])
		}
	}
	var buf bytes.Buffer
	if _, err := cfg.WriteTo(&buf); err != nil {
		return "", fmt.Errorf("ini driver: marshal: %w", err)
	}
	return buf.String(), nil
}

func extractINI(content string, keys []string) (map[string]string, error) {
	cfg, err := ini.Load([]byte(content))
	if err != nil {
		return nil, fmt.Errorf("ini driver: parse: %w", err)
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		section, key, err := splitINIPath(k)
		if err != nil {
			return nil, fmt.Errorf("ini driver: %w", err)
		}
		s, err := cfg.GetSection(section)
		if err != nil {
			continue
		}
		if !s.HasKey(key) {
			continue
		}
		out[k] = s.Key(key).Value()
	}
	return out, nil
}

// ─── remove (per-driver) ─────────────────────────────────────────────
//
// Each remover deletes the named keys from `content`, idempotent on
// missing keys. Called by Clean for any `patches[].set` key that
// HEAD doesn't define — strip the appended form so `clean(working)
// == HEAD` and the file stops showing as dirty.

func removeDotenvContent(content string, keys []string) string {
	for _, k := range keys {
		// `(?m)^KEY\s*=.*\n?` — match a whole line including its
		// trailing newline if present. Trailing-newline optional so
		// the last line of a file without final \n still strips
		// cleanly.
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(k) + `\s*=.*\n?`)
		content = re.ReplaceAllString(content, "")
	}
	return content
}

func removePhpunitContent(content string, keys []string) string {
	for _, k := range keys {
		// Match `<env name="KEY" .../>` together with all leading
		// horizontal whitespace on the line and the trailing newline
		// Apply uses when injecting (`\t<env .../>\n`). `[ \t]*`
		// (greedy) absorbs hand-edited indent depth — using `\t?`
		// strips only one tab and leaves a dangling indent on the
		// next line. Trailing `\n?` is optional so the last line of
		// a file with no final newline strips cleanly too.
		re := regexp.MustCompile(`(?m)^[ \t]*<env name="` + regexp.QuoteMeta(k) + `"[^/]*/>\n?`)
		content = re.ReplaceAllString(content, "")
	}
	return content
}

func removeYAMLContent(content string, keys []string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("yaml driver: parse: %w", err)
	}
	for _, k := range keys {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return "", fmt.Errorf("yaml driver: path %q: %w", k, err)
		}
		yamlDeleteAt(&doc, segs)
	}
	body, err := yamlpatch.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("yaml driver: marshal: %w", err)
	}
	return string(body), nil
}

// yamlDeleteAt walks `segs` and removes the terminal key from its
// parent mapping (or the terminal index from its parent sequence).
// Missing paths are a no-op — symmetric with Apply's "create
// intermediate" semantics; removing a path that was never there is
// the desired clean outcome for the "key absent from HEAD AND from
// working" race.
func yamlDeleteAt(doc *yaml.Node, segs []yamlpatch.Segment) {
	if len(segs) == 0 {
		return
	}
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	// Walk to parent of terminal segment.
	for i := 0; i < len(segs)-1; i++ {
		seg := segs[i]
		switch {
		case seg.IsIndex:
			if n.Kind != yaml.SequenceNode || seg.Idx < 0 || seg.Idx >= len(n.Content) {
				return
			}
			n = n.Content[seg.Idx]
		default:
			if n.Kind != yaml.MappingNode {
				return
			}
			found := false
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == seg.Key {
					n = n.Content[i+1]
					found = true
					break
				}
			}
			if !found {
				return
			}
		}
	}
	last := segs[len(segs)-1]
	if last.IsIndex {
		if n.Kind != yaml.SequenceNode || last.Idx < 0 || last.Idx >= len(n.Content) {
			return
		}
		n.Content = append(n.Content[:last.Idx], n.Content[last.Idx+1:]...)
		return
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	// Mapping nodes store pairs as `[key0, val0, key1, val1, ...]`;
	// removing both elements keeps the slice valid.
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == last.Key {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return
		}
	}
}

func removeJSONContent(content string, keys []string) (string, error) {
	var doc any
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("json driver: parse: %w", err)
	}
	for _, k := range keys {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return "", fmt.Errorf("json driver: path %q: %w", k, err)
		}
		deleteJSONPath(doc, segs)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("json driver: marshal: %w", err)
	}
	return string(out) + "\n", nil
}

func removeTOMLContent(content string, keys []string) (string, error) {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(content), &doc); err != nil {
		return "", fmt.Errorf("toml driver: parse: %w", err)
	}
	for _, k := range keys {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return "", fmt.Errorf("toml driver: path %q: %w", k, err)
		}
		deleteJSONPath(any(doc), segs)
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(doc); err != nil {
		return "", fmt.Errorf("toml driver: marshal: %w", err)
	}
	return buf.String(), nil
}

// deleteJSONPath removes the terminal segment from a JSON / TOML
// document. Shared by both drivers because go-toml/v2 decodes to
// `map[string]any` / `[]any` — same shape as encoding/json.
func deleteJSONPath(root any, segs []yamlpatch.Segment) {
	if len(segs) == 0 {
		return
	}
	parent := root
	for i := 0; i < len(segs)-1; i++ {
		seg := segs[i]
		switch p := parent.(type) {
		case map[string]any:
			if seg.IsIndex {
				return
			}
			next, ok := p[seg.Key]
			if !ok {
				return
			}
			parent = next
		case []any:
			if !seg.IsIndex || seg.Idx < 0 || seg.Idx >= len(p) {
				return
			}
			parent = p[seg.Idx]
		default:
			return
		}
	}
	last := segs[len(segs)-1]
	switch p := parent.(type) {
	case map[string]any:
		if last.IsIndex {
			return
		}
		delete(p, last.Key)
		// Note: slice-typed terminals can't be removed in place without
		// re-pointing the parent. Treeman patches only ever target
		// scalar leaves in `patches[].set`, so this branch isn't
		// reachable; if a future driver path needs it, surface the
		// parent + index up here.
	}
}

func removeINIContent(content string, keys []string) (string, error) {
	cfg, err := ini.Load([]byte(content))
	if err != nil {
		return "", fmt.Errorf("ini driver: parse: %w", err)
	}
	for _, k := range keys {
		section, key, err := splitINIPath(k)
		if err != nil {
			return "", fmt.Errorf("ini driver: %w", err)
		}
		s, err := cfg.GetSection(section)
		if err != nil {
			continue
		}
		s.DeleteKey(key)
	}
	var buf bytes.Buffer
	if _, err := cfg.WriteTo(&buf); err != nil {
		return "", fmt.Errorf("ini driver: marshal: %w", err)
	}
	return buf.String(), nil
}
