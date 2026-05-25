package patcher

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	ini "gopkg.in/ini.v1"

	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// PatchTOMLFile sets each (dotted-path, value) pair inside the TOML
// document at `path`. Uses pelletier/go-toml/v2 — comments are NOT
// preserved (limitation of the v2 marshaller). Round-trips through
// map[string]any.
//
// Path syntax matches yamlpatch (`tool.poetry.name`, `array[0].x`).
// Integer-shaped values are written as TOML integers; everything
// else as strings.
func PatchTOMLFile(path string, pairs map[string]string) (Outcome, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Missing, nil
		}
		return Missing, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(body, &doc); err != nil {
		return Missing, fmt.Errorf("parse %s: %w", path, err)
	}
	root := any(doc)
	changed := false
	for _, k := range sortedKeys(pairs) {
		segs, err := yamlpatch.ParsePath(k)
		if err != nil {
			return Missing, fmt.Errorf("toml driver: path %q: %w", k, err)
		}
		newVal := tomlScalar(pairs[k])
		prev, err := setJSONPath(root, segs, newVal)
		if err != nil {
			return Missing, fmt.Errorf("toml driver: set %q: %w", k, err)
		}
		if !jsonEqual(prev, newVal) {
			changed = true
		}
	}
	if !changed {
		return Unchanged, nil
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(doc); err != nil {
		return Updated, fmt.Errorf("toml driver: marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return Updated, fmt.Errorf("write %s: %w", path, err)
	}
	return Updated, nil
}

// tomlScalar mirrors jsonScalar — purely numeric strings become
// int64s so e.g. `port: "5432"` lands as a TOML integer. Booleans
// stay strings because the user can write `true` in the template and
// surprise themselves.
func tomlScalar(v string) any {
	if v == "" {
		return v
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	return v
}

// PatchINIFile sets each (path, value) pair inside the INI document
// at `path`. Path syntax is `section.key`; a single-segment path
// (`key`) targets the default (unnamed) section. Uses gopkg.in/ini.v1
// which DOES preserve comments and key ordering across reads/writes.
func PatchINIFile(path string, pairs map[string]string) (Outcome, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return Missing, nil
		}
		return Missing, fmt.Errorf("stat %s: %w", path, err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return Missing, fmt.Errorf("read %s: %w", path, err)
	}
	cfg, err := ini.Load(original)
	if err != nil {
		return Missing, fmt.Errorf("parse %s: %w", path, err)
	}
	changed := false
	for _, k := range sortedKeys(pairs) {
		section, key, err := splitINIPath(k)
		if err != nil {
			return Missing, fmt.Errorf("ini driver: %w", err)
		}
		s, err := cfg.GetSection(section)
		if err != nil {
			s, _ = cfg.NewSection(section)
		}
		if !s.HasKey(key) {
			_, _ = s.NewKey(key, pairs[k])
			changed = true
			continue
		}
		existing := s.Key(key).Value()
		if existing != pairs[k] {
			s.Key(key).SetValue(pairs[k])
			changed = true
		}
	}
	if !changed {
		return Unchanged, nil
	}
	var buf bytes.Buffer
	if _, err := cfg.WriteTo(&buf); err != nil {
		return Updated, fmt.Errorf("ini driver: marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return Updated, fmt.Errorf("write %s: %w", path, err)
	}
	return Updated, nil
}

// splitINIPath turns `section.key` → ("section", "key"). A flat
// `key` lands in the unnamed default section ("DEFAULT" in ini.v1).
func splitINIPath(p string) (section, key string, err error) {
	if p == "" {
		return "", "", fmt.Errorf("empty ini path")
	}
	parts := strings.Split(p, ".")
	if len(parts) == 1 {
		return ini.DefaultSection, parts[0], nil
	}
	if len(parts) > 2 {
		return "", "", fmt.Errorf("ini path %q: deeper than `section.key` not supported (use yaml/json/toml for nested)", p)
	}
	return parts[0], parts[1], nil
}
