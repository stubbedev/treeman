package patcher

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	ini "gopkg.in/ini.v1"
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
	// Byte-preserving value splice for existing keys (create falls back
	// to the reformatting marshal). Keeps clean(apply(HEAD)) == HEAD so
	// the patched file never shows as modified in `git status`.
	out, changed, err := setTOMLInPlace(string(body), pairs)
	if err != nil {
		return Missing, fmt.Errorf("toml driver %s: %w", path, err)
	}
	if !changed {
		return Unchanged, nil
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
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
	// Byte-preserving value splice for existing keys (create falls back
	// to ini.WriteTo). Keeps clean(apply(HEAD)) == HEAD so the patched
	// file never shows as modified in `git status` — ini.WriteTo
	// otherwise normalizes `k=v` → `k = v` on every key.
	out, changed, err := setINIInPlace(string(original), pairs)
	if err != nil {
		return Missing, fmt.Errorf("ini driver %s: %w", path, err)
	}
	if !changed {
		return Unchanged, nil
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
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
