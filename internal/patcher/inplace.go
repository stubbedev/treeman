// In-place value splicing for the json / toml / ini drivers.
//
// The marshal-based apply (json.MarshalIndent, toml encoder, ini WriteTo)
// is NOT byte-stable: it reorders JSON object keys alphabetically and
// normalizes INI/TOML spacing. That breaks the clean/smudge round-trip —
// clean projects the patched value back to HEAD but can't undo the
// whole-file reformat, so a patched config shows as permanently modified
// in `git status` (KON git-dirty class of bugs, all of json/toml/ini).
//
// These setters splice ONLY the patched scalar's bytes into the existing
// content, leaving every other byte (key order, indentation, comments,
// unrelated keys) untouched — so for an existing key the round-trip is
// byte-stable and git stays clean. Creating a key that HEAD lacks falls
// back to the marshal path (the file legitimately gains content, so it
// differs from HEAD regardless; clean's removeContent strips it).
package patcher

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// setJSONInPlace splices each present scalar value in place. Falls back
// to the marshal path if any key is absent or non-scalar (create case).
// Returns (out, changed, error).
func setJSONInPlace(content string, pairs map[string]string) (string, bool, error) {
	out := content
	for _, k := range sortedKeys(pairs) {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return "", false, err
		}
		s, e, ok := jsonValueRange(out, segs)
		if !ok {
			return jsonMarshalFallback(content, pairs)
		}
		// jsonValueRange's range starts at the separator preceding the
		// value (`:` for object members, `,` for array elements) — clean
		// splices that range symmetrically, but apply writes a bare value
		// literal, so advance past the separator + spacing to the value
		// itself, leaving the separator (and its spacing) intact.
		for s < e {
			switch out[s] {
			case ':', ',', ' ', '\t', '\n', '\r':
				s++
				continue
			}
			break
		}
		lit := jsonLiteralFor(pairs[k])
		out = out[:s] + lit + out[e:]
	}
	return out, out != content, nil
}

func jsonMarshalFallback(content string, pairs map[string]string) (string, bool, error) {
	out, err := applyJSONMarshal(content, pairs)
	if err != nil {
		return "", false, err
	}
	return out, out != content, nil
}

// jsonLiteralFor renders a patched value as the JSON token to splice —
// the same string/number coercion the marshal path applies (jsonScalar),
// then json.Marshal so quoting/escaping matches what HEAD would carry.
func jsonLiteralFor(v string) string {
	b, err := json.Marshal(jsonScalar(v))
	if err != nil {
		// jsonScalar only ever yields string/int, both of which marshal;
		// fall back to a quoted string on the impossible error path.
		b, _ = json.Marshal(v)
	}
	return string(b)
}

// setTOMLInPlace splices each present `key = …` value line within its
// table. Falls back to marshal for absent tables/keys.
func setTOMLInPlace(content string, pairs map[string]string) (string, bool, error) {
	out := content
	for _, k := range sortedKeys(pairs) {
		dot := strings.LastIndexByte(k, '.')
		table, key := "", k
		if dot >= 0 {
			table, key = k[:dot], k[dot+1:]
		}
		sec, start, end := tomlSection(out, table)
		if sec == "" {
			return tomlMarshalFallback(content, pairs)
		}
		newSec, found := spliceAssignValue(sec, key, tomlLiteralFor(pairs[k]))
		if !found {
			return tomlMarshalFallback(content, pairs)
		}
		out = out[:start] + newSec + out[end:]
	}
	return out, out != content, nil
}

func tomlMarshalFallback(content string, pairs map[string]string) (string, bool, error) {
	out, err := applyTOMLMarshal(content, pairs)
	if err != nil {
		return "", false, err
	}
	return out, out != content, nil
}

// tomlLiteralFor mirrors tomlScalar: integer-shaped values stay bare;
// everything else is a basic string. strconv.Quote produces a valid
// TOML basic string for the slug-shaped values treeman writes.
func tomlLiteralFor(v string) string {
	if v == "" {
		return `""`
	}
	if _, err := strconv.ParseInt(v, 10, 64); err == nil {
		return v
	}
	return strconv.Quote(v)
}

// setINIInPlace splices each present `key = …` value line within its
// section. Falls back to marshal for absent sections/keys.
func setINIInPlace(content string, pairs map[string]string) (string, bool, error) {
	out := content
	for _, k := range sortedKeys(pairs) {
		section, key, err := splitINIPath(k)
		if err != nil {
			return "", false, err
		}
		sec, start, end := iniSection(out, section)
		if sec == "" {
			return iniMarshalFallback(content, pairs)
		}
		newSec, found := spliceAssignValue(sec, key, pairs[k])
		if !found {
			return iniMarshalFallback(content, pairs)
		}
		out = out[:start] + newSec + out[end:]
	}
	return out, out != content, nil
}

func iniMarshalFallback(content string, pairs map[string]string) (string, bool, error) {
	out, err := applyINIMarshal(content, pairs)
	if err != nil {
		return "", false, err
	}
	return out, out != content, nil
}

// spliceAssignValue replaces the value portion of a `key = …` (or
// `key=…`) line inside `section`, preserving the key, the exact spacing
// around `=`, and the rest of the section. Returns (newSection, found).
// Used for both TOML and INI — same line grammar.
func spliceAssignValue(section, key, literal string) (string, bool) {
	re := regexp.MustCompile(`(?m)^(\s*` + regexp.QuoteMeta(key) + `\s*=\s*).*$`)
	loc := re.FindStringSubmatchIndex(section)
	if loc == nil {
		return section, false
	}
	// loc[2]:loc[3] is capture group 1 (the `key = ` prefix incl. indent
	// + spacing); loc[0]:loc[1] is the whole matched line.
	prefix := section[loc[2]:loc[3]]
	newLine := prefix + literal
	return section[:loc[0]] + newLine + section[loc[1]:], true
}
