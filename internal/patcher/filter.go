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

// reSectionHeader matches an INI/TOML `[section]` header at line start.
// Hoisted to package scope because iniSection/tomlSection are on the
// hot clean/smudge path (they run on every `git status`/`git add` of a
// patched file) and previously recompiled this constant pattern on
// every call.
var reSectionHeader = regexp.MustCompile(`(?m)^\[`)

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
	present := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := headVals[k]; ok {
			present = append(present, k)
		} else {
			absent = append(absent, k)
		}
	}
	if len(absent) > 0 {
		content, err = removeContent(driver, content, absent)
		if err != nil {
			return "", err
		}
	}
	if len(present) == 0 {
		return content, nil
	}
	return restoreContent(driver, content, headContent, headVals, present)
}

// restoreContent projects HEAD's exact bytes for each present key
// back into `content` so clean(working) matches HEAD byte-for-byte
// for the patched keys, regardless of any formatting differences
// Apply's render path would introduce (`force="true"` injection,
// `KEY=val` vs `KEY = val` spacing, alphabetic key reorder on
// json/toml marshal, quoting-style swaps on yaml marshal, etc.).
//
// All drivers use HEAD-byte splicing now rather than the
// parser-render round-trip. The latter was the source of the
// "patched file shows as modified on every status" class of bugs:
// even when HEAD's scalar matched what clean wanted to restore, the
// re-rendered bytes drifted from HEAD because Apply (the only
// renderer) imposes its own canonical form.
//
// Per-driver strategy:
//
//   - dotenv / ini / toml / phpunit — line- or element-level regex
//     splice. The grammar is line-stable enough that "find KEY's
//     line in HEAD, drop it into the same place in content"
//     produces HEAD-equal output.
//   - yaml / json — same idea, parser-assisted. Regex finds the
//     leaf-key occurrence; the result preserves quoting + ordering
//     because we never re-marshal. For deeply-nested paths where
//     the key name isn't unique, we fall through to `applyContent`
//     and document the round-trip caveat.
//
// `headVals` is provided so the fallback path can still call into
// applyContent with HEAD's scalar values; `present` lists the keys
// that exist in HEAD (i.e. need restore, not strip).
func restoreContent(driver, content, headContent string, headVals map[string]string, present []string) (string, error) {
	switch driver {
	case "phpunit":
		return restorePhpunitFromHead(content, headContent, present), nil
	case "dotenv":
		return restoreDotenvFromHead(content, headContent, present), nil
	case "ini":
		return restoreINIFromHead(content, headContent, present), nil
	case "toml":
		return restoreTOMLFromHead(content, headContent, present), nil
	case "yaml":
		return restoreYAMLFromHead(content, headContent, headVals, present)
	case "json":
		return restoreJSONFromHead(content, headContent, headVals, present)
	}
	subset := make(map[string]string, len(present))
	for _, k := range present {
		subset[k] = headVals[k]
	}
	return applyContent(driver, content, subset)
}

// ─── per-driver HEAD-byte splicing ───────────────────────────────────

// restorePhpunitFromHead rewrites each `<env name="K" .../>` element
// in `content` to match HEAD's exact bytes for that K. Without this,
// `force="true"` added by Apply (always present in the smudge output)
// leaks into the clean output for any HEAD entry that didn't already
// carry the attribute.
func restorePhpunitFromHead(content, headContent string, keys []string) string {
	for _, k := range keys {
		re := regexp.MustCompile(`<env name="` + regexp.QuoteMeta(k) + `"[^/]*/>`)
		headElem := re.FindString(headContent)
		if headElem == "" {
			continue
		}
		// ReplaceAllLiteralString avoids `$1`/`${name}` expansion in
		// case HEAD's element bytes contain `$`.
		content = re.ReplaceAllLiteralString(content, headElem)
	}
	return content
}

// restoreDotenvFromHead splices HEAD's `KEY=...` line into `content`
// for each key. Catches `KEY = val` ↔ `KEY=val` spacing drift,
// double-quoted vs unquoted, etc. — Apply only writes the
// `KEY=val` form, so HEAD with any other shape would otherwise show
// as permanently modified.
func restoreDotenvFromHead(content, headContent string, keys []string) string {
	for _, k := range keys {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(k) + `\s*=.*$`)
		headLine := re.FindString(headContent)
		if headLine == "" {
			continue
		}
		content = re.ReplaceAllLiteralString(content, headLine)
	}
	return content
}

// restoreINIFromHead splices HEAD's `key=...` line into `content`
// per `<section>.<key>` dotted key. Section scoping prevents a
// `[other]` block's identically-named key from being matched. Apply
// goes through go-ini's WriteTo which can normalize spacing — this
// path preserves whatever shape HEAD has.
func restoreINIFromHead(content, headContent string, dottedKeys []string) string {
	for _, dk := range dottedKeys {
		section, key, err := splitINIPath(dk)
		if err != nil {
			continue
		}
		// Pluck the section bodies from HEAD and content; line-splice
		// inside, then stitch back.
		headSec, headStart, headEnd := iniSection(headContent, section)
		curSec, curStart, curEnd := iniSection(content, section)
		if headSec == "" || curSec == "" {
			continue
		}
		// Allow leading indentation so an indented `  k = v` key — which
		// go-ini (extractINI) happily reports as present — is matched here
		// too. Without the `[ \t]*` prefix the splice would silently miss
		// it, leaving the smudged value in place and the file permanently
		// git-dirty. Mirrors spliceAssignValue's `^\s*key` on the apply side.
		lineRe := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(key) + `\s*=.*$`)
		headLine := lineRe.FindString(headSec)
		if headLine == "" {
			continue
		}
		newSec := lineRe.ReplaceAllLiteralString(curSec, headLine)
		content = content[:curStart] + newSec + content[curEnd:]
		_ = headStart
		_ = headEnd
	}
	return content
}

// iniSection returns the body of `[section]` (start..end byte
// offsets exclusive of the header line, up to the next header or
// EOF). Empty string + (-1, -1) when the section isn't found. The
// implicit "DEFAULT" / unnamed top-level section is named "" (go-ini
// convention); we match the unnamed section as everything before
// the first `^\[`.
func iniSection(content, section string) (string, int, int) {
	if section == "" || strings.EqualFold(section, "DEFAULT") {
		// Body before first explicit [section].
		loc := reSectionHeader.FindStringIndex(content)
		if loc == nil {
			return content, 0, len(content)
		}
		return content[:loc[0]], 0, loc[0]
	}
	// Header line `[<section>]` (allow trailing whitespace / comment).
	headerRe := regexp.MustCompile(`(?m)^\[` + regexp.QuoteMeta(section) + `\][^\n]*\n`)
	loc := headerRe.FindStringIndex(content)
	if loc == nil {
		return "", -1, -1
	}
	bodyStart := loc[1]
	// Next section header or EOF.
	nextLoc := reSectionHeader.FindStringIndex(content[bodyStart:])
	bodyEnd := len(content)
	if nextLoc != nil {
		bodyEnd = bodyStart + nextLoc[0]
	}
	return content[bodyStart:bodyEnd], bodyStart, bodyEnd
}

// restoreTOMLFromHead splices HEAD's `key = ...` line into `content`
// per `<table>.<key>` dotted key. Top-level keys (no dot) map to the
// implicit "" table — the body before the first `[table]` header.
// Dotted paths name an explicit `[table]` block. Nested tables
// (`[a.b]`) work via the literal section name in the patch path
// (`a.b.k`).
func restoreTOMLFromHead(content, headContent string, dottedKeys []string) string {
	for _, dk := range dottedKeys {
		dot := strings.LastIndexByte(dk, '.')
		var table, key string
		if dot < 0 {
			table, key = "", dk
		} else {
			table, key = dk[:dot], dk[dot+1:]
		}
		headSec, _, _ := tomlSection(headContent, table)
		curSec, curStart, curEnd := tomlSection(content, table)
		if headSec == "" || curSec == "" {
			continue
		}
		// Indentation-tolerant for the same reason as restoreINIFromHead:
		// TOML permits indented keys and go-toml (extractTOML) accepts them.
		lineRe := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(key) + `\s*=.*$`)
		headLine := lineRe.FindString(headSec)
		if headLine == "" {
			continue
		}
		newSec := lineRe.ReplaceAllLiteralString(curSec, headLine)
		content = content[:curStart] + newSec + content[curEnd:]
	}
	return content
}

// tomlSection is the TOML analogue of iniSection: locate `[table]`
// (or the pre-header body for the implicit "" table) and return
// body + offsets. Inline tables (`a = { b = c }`) are not handled
// — patch paths targeting inline-table leaves fall through to
// applyContent.
func tomlSection(content, table string) (string, int, int) {
	if table == "" {
		loc := reSectionHeader.FindStringIndex(content)
		if loc == nil {
			return content, 0, len(content)
		}
		return content[:loc[0]], 0, loc[0]
	}
	headerRe := regexp.MustCompile(`(?m)^\[` + regexp.QuoteMeta(table) + `\][^\n]*\n`)
	loc := headerRe.FindStringIndex(content)
	if loc == nil {
		return "", -1, -1
	}
	bodyStart := loc[1]
	nextLoc := reSectionHeader.FindStringIndex(content[bodyStart:])
	bodyEnd := len(content)
	if nextLoc != nil {
		bodyEnd = bodyStart + nextLoc[0]
	}
	return content[bodyStart:bodyEnd], bodyStart, bodyEnd
}

// restoreYAMLFromHead splices HEAD's full `<key>: …` line (or block)
// into `content` for each leaf. We anchor on the parent map's
// position via yaml.Node.Line so nested paths (`a.b.c`) still hit
// the right occurrence — `(?m)^<indent><leaf>:` is too ambiguous for
// nested docs.
//
// Multi-line scalars (`>`/`|`) and mapping/sequence values aren't
// scoped here; the splice falls back to applyContent for those —
// caller still benefits from value-restore but format drift can
// surface. Documented as a known limitation.
func restoreYAMLFromHead(content, headContent string, headVals map[string]string, present []string) (string, error) {
	fallback := []string{}
	// HEAD is immutable across the loop, so parse it once rather than
	// once per key (was 2N parses for N keys). `content` is mutated by
	// each splice, shifting byte offsets, so it must still be reparsed
	// per key.
	headRoot, headOK := yamlRootNode(headContent)
	for _, k := range present {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			fallback = append(fallback, k)
			continue
		}
		if !headOK {
			fallback = append(fallback, k)
			continue
		}
		hLine, _, _, ok := yamlScalarLineBytesNode(headContent, headRoot, segs)
		if !ok {
			fallback = append(fallback, k)
			continue
		}
		curRoot, ok := yamlRootNode(content)
		if !ok {
			fallback = append(fallback, k)
			continue
		}
		_, cStart, cEnd, ok := yamlScalarLineBytesNode(content, curRoot, segs)
		if !ok {
			fallback = append(fallback, k)
			continue
		}
		content = content[:cStart] + hLine + content[cEnd:]
	}
	if len(fallback) > 0 {
		subset := make(map[string]string, len(fallback))
		for _, k := range fallback {
			if v, ok := headVals[k]; ok {
				subset[k] = v
			}
		}
		out, err := applyContent("yaml", content, subset)
		if err != nil {
			return "", err
		}
		content = out
	}
	return content, nil
}

// yamlRootNode parses `doc` and returns its top content node (the
// document node unwrapped). False if the doc doesn't parse or is empty.
// Split out from yamlScalarLineBytesNode so callers can parse an
// immutable document once and walk it for many keys.
func yamlRootNode(doc string) (*yaml.Node, bool) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		return nil, false
	}
	n := &root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	return n, true
}

// yamlScalarLineBytesNode walks a pre-parsed `root` (from yamlRootNode
// over `doc`) along `segs` to the terminal scalar, and returns that
// line's bytes + the start/end offsets in `doc`. The returned line
// includes the leading indent so a splice of head-line into
// content-line preserves indent depth. Empty + false when the terminal
// isn't a scalar (mapping/sequence terminals are unsupported — they'd
// require multi-line block detection).
func yamlScalarLineBytesNode(doc string, root *yaml.Node, segs []yamlpatch.Segment) (string, int, int, bool) {
	n := root
	for _, seg := range segs {
		switch {
		case seg.IsIndex:
			if n.Kind != yaml.SequenceNode || seg.Idx < 0 || seg.Idx >= len(n.Content) {
				return "", 0, 0, false
			}
			n = n.Content[seg.Idx]
		default:
			if n.Kind != yaml.MappingNode {
				return "", 0, 0, false
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
				return "", 0, 0, false
			}
		}
	}
	if n.Kind != yaml.ScalarNode {
		return "", 0, 0, false
	}
	// Line in yaml.Node is 1-based; convert to byte range of the
	// containing line in `doc`.
	lineStart, lineEnd := lineByteRange(doc, n.Line)
	if lineStart < 0 {
		return "", 0, 0, false
	}
	return doc[lineStart:lineEnd], lineStart, lineEnd, true
}

// lineByteRange returns (start, end) byte offsets of `line1` (1-based)
// in `s`, inclusive of the terminating newline if present. (-1, -1)
// for out-of-range.
func lineByteRange(s string, line1 int) (int, int) {
	if line1 < 1 {
		return -1, -1
	}
	cur := 1
	start := 0
	for i := range len(s) {
		if cur == line1 && s[i] == '\n' {
			return start, i + 1
		}
		if s[i] == '\n' {
			cur++
			start = i + 1
		}
	}
	if cur == line1 {
		return start, len(s)
	}
	return -1, -1
}

// restoreJSONFromHead splices HEAD's value-token bytes back into
// `content` for each leaf scalar key. Walks the JSON in both
// documents via a tokenizer that tracks byte offsets so we can splice
// without re-marshaling (json.MarshalIndent reorders keys
// alphabetically and forces multi-line output — neither matches an
// arbitrary HEAD).
//
// Falls back to applyContent for keys we can't locate in either
// side; that path will reformat the document but at least produces
// the right scalar value.
func restoreJSONFromHead(content, headContent string, headVals map[string]string, present []string) (string, error) {
	fallback := []string{}
	for _, k := range present {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			fallback = append(fallback, k)
			continue
		}
		hBytes, ok := jsonValueBytes(headContent, segs)
		if !ok {
			fallback = append(fallback, k)
			continue
		}
		cStart, cEnd, ok := jsonValueRange(content, segs)
		if !ok {
			fallback = append(fallback, k)
			continue
		}
		content = content[:cStart] + hBytes + content[cEnd:]
	}
	if len(fallback) > 0 {
		subset := make(map[string]string, len(fallback))
		for _, k := range fallback {
			if v, ok := headVals[k]; ok {
				subset[k] = v
			}
		}
		out, err := applyContent("json", content, subset)
		if err != nil {
			return "", err
		}
		content = out
	}
	return content, nil
}

// jsonValueRange walks the JSON in `doc` and returns the byte range
// of the value token at the path `segs`. Uses encoding/json's
// Decoder + InputOffset to track positions without re-marshaling.
// Returns (-1, -1, false) when the path doesn't terminate at a
// scalar value.
//
// Algorithm: maintain a stack of `(isArr, pendingKey|nextIdx)` —
// one entry per container we're currently inside. A value position
// is "the value about to be emitted by Decoder.Token()". When the
// stack depth equals len(segs) and every entry matches its
// corresponding segment, the next token is the target value.
func jsonValueRange(doc string, segs []yamlpatch.Segment) (int, int, bool) {
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var stack []jsonStackEntry

	for {
		startOff := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return -1, -1, false
		}
		if d, ok := tok.(json.Delim); ok {
			next, ok := jsonHandleDelim(stack, segs, d)
			if !ok {
				return -1, -1, false
			}
			stack = next
			continue
		}
		// Scalar token. Inside an object, before the value comes a key
		// (also a string token); pendingKey == "" means we're expecting
		// the key, then any subsequent token is the value.
		if len(stack) > 0 && !stack[len(stack)-1].isArr && stack[len(stack)-1].pendingKey == "" {
			if s, ok := tok.(string); ok {
				stack[len(stack)-1].pendingKey = s
			}
			continue
		}
		// Value token. Check if it sits at the requested path.
		if jsonStackMatches(stack, segs) {
			endOff := dec.InputOffset()
			s := int(startOff)
			e := int(endOff)
			for s < e && (doc[s] == ' ' || doc[s] == '\t' || doc[s] == '\n' || doc[s] == '\r') {
				s++
			}
			return s, e, true
		}
		jsonAdvanceTop(stack)
	}
}

// jsonStackEntry is one container frame in jsonValueRange's walk.
type jsonStackEntry struct {
	isArr      bool
	pendingKey string // object: key whose value comes next; "" before key read
	nextIdx    int    // array: index of value about to be emitted
}

// jsonStackMatches reports whether the current container stack exactly
// corresponds to the requested path segments.
func jsonStackMatches(stack []jsonStackEntry, segs []yamlpatch.Segment) bool {
	if len(stack) != len(segs) {
		return false
	}
	for i, e := range stack {
		seg := segs[i]
		if e.isArr != seg.IsIndex {
			return false
		}
		if seg.IsIndex {
			if e.nextIdx != seg.Idx {
				return false
			}
		} else {
			if e.pendingKey != seg.Key {
				return false
			}
		}
	}
	return true
}

// jsonAdvanceTop moves the top container frame's pointer past a value
// that has just been consumed.
func jsonAdvanceTop(stack []jsonStackEntry) {
	if len(stack) == 0 {
		return
	}
	top := &stack[len(stack)-1]
	if top.isArr {
		top.nextIdx++
	} else {
		top.pendingKey = ""
	}
}

// jsonHandleDelim processes an open/close delimiter token, returning the
// updated stack. ok is false when the document terminated at a container
// (caller wanted a scalar) or the close delim has no matching open.
func jsonHandleDelim(stack []jsonStackEntry, segs []yamlpatch.Segment, d json.Delim) ([]jsonStackEntry, bool) {
	if d == '{' || d == '[' {
		// Container opens. If we're at the target path, the
		// requested leaf is a container — caller wanted a scalar
		// so bail (current treeman patch leaves are always scalar).
		if jsonStackMatches(stack, segs) {
			return stack, false
		}
		return append(stack, jsonStackEntry{isArr: d == '['}), true
	}
	// Close delim — pop, then advance the parent's pointer
	// past the just-closed value.
	if len(stack) == 0 {
		return stack, false
	}
	stack = stack[:len(stack)-1]
	jsonAdvanceTop(stack)
	return stack, true
}

// jsonValueBytes returns the literal byte slice (as a string) of the
// JSON value at `segs` in `doc`. Thin wrapper over jsonValueRange.
func jsonValueBytes(doc string, segs []yamlpatch.Segment) (string, bool) {
	s, e, ok := jsonValueRange(doc, segs)
	if !ok {
		return "", false
	}
	return doc[s:e], true
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

// applyJSONContent rewrites the patched values, preserving the file's
// byte layout (key order, indentation) for keys that already exist so
// the clean/smudge round-trip stays byte-stable and git sees no
// modification. Only a create-missing-key falls through to the
// reordering marshal path.
func applyJSONContent(content string, pairs map[string]string) (string, error) {
	out, _, err := setJSONInPlace(content, pairs)
	return out, err
}

func applyJSONMarshal(content string, pairs map[string]string) (string, error) {
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
		if err := setJSONPath(root, segs, newVal); err != nil {
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

// applyTOMLContent — byte-preserving value splice for existing keys;
// see applyJSONContent. Marshal fallback (applyTOMLMarshal) only for a
// create-missing-key.
func applyTOMLContent(content string, pairs map[string]string) (string, error) {
	out, _, err := setTOMLInPlace(content, pairs)
	return out, err
}

func applyTOMLMarshal(content string, pairs map[string]string) (string, error) {
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
		if err := setJSONPath(root, segs, tomlScalar(pairs[k])); err != nil {
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

// applyINIContent — byte-preserving value splice for existing keys; see
// applyJSONContent. Marshal fallback (applyINIMarshal) only for a
// create-missing-key.
func applyINIContent(content string, pairs map[string]string) (string, error) {
	out, _, err := setINIInPlace(content, pairs)
	return out, err
}

func applyINIMarshal(content string, pairs map[string]string) (string, error) {
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
	n, ok := yamlWalkToParent(n, segs)
	if !ok {
		return
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

// yamlWalkToParent descends n along all but the last path segment,
// returning the parent node of the terminal segment. ok is false when
// any intermediate segment is missing or the node kind doesn't match.
func yamlWalkToParent(n *yaml.Node, segs []yamlpatch.Segment) (*yaml.Node, bool) {
	for i := 0; i < len(segs)-1; i++ {
		seg := segs[i]
		switch {
		case seg.IsIndex:
			if n.Kind != yaml.SequenceNode || seg.Idx < 0 || seg.Idx >= len(n.Content) {
				return nil, false
			}
			n = n.Content[seg.Idx]
		default:
			if n.Kind != yaml.MappingNode {
				return nil, false
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
				return nil, false
			}
		}
	}
	return n, true
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
	for i := range len(segs) - 1 {
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
	// Note: slice-typed terminals can't be removed in place without
	// re-pointing the parent. Treeman patches only ever target
	// scalar leaves in `patches[].set`, so that branch isn't
	// reachable; if a future driver path needs it, surface the
	// parent + index up here.
	if p, ok := parent.(map[string]any); ok {
		if last.IsIndex {
			return
		}
		delete(p, last.Key)
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
