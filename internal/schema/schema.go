// Package schema renders the JSON Schema for `.treeman.yaml` and
// manages the yaml-language-server modeline that wires the schema
// into editor autocompletion.
//
// Lives outside cmd/mcp so both surfaces share one implementation:
// `treeman schema install` (CLI) and the MCP `schema_install` tool
// both call into here so neither needs to shell out and they can
// never drift.
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/invopop/jsonschema"

	"github.com/stubbedev/treeman/internal/config"
)

// URL is the canonical upstream URL for the generated schema. Used
// by `treeman init` (modeline default) and the `--url` install mode.
const URL = "https://raw.githubusercontent.com/stubbedev/treeman/master/schemas/treeman.schema.json"

// Render returns the JSON Schema for config.Config as pretty-printed
// bytes. Generated via reflection so adding a Go field automatically
// flows into editor hinting.
func Render() ([]byte, error) {
	r := &jsonschema.Reflector{
		Anonymous:      true,
		ExpandedStruct: true,
		FieldNameTag:   "yaml",
	}
	return json.MarshalIndent(r.Reflect(&config.Config{}), "", "  ")
}

// GlobalPath returns the OS-conventional user-global path
// (`$XDG_CONFIG_HOME/treeman/treeman.schema.json` on Linux,
// `~/Library/Application Support/treeman/...` on macOS, …).
func GlobalPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "treeman", "treeman.schema.json"), nil
}

// Target enumerates the three install locations.
type Target int

const (
	// TargetRepo writes `<repoRoot>/schemas/treeman.schema.json`.
	TargetRepo Target = iota
	// TargetGlobal writes to GlobalPath().
	TargetGlobal
	// TargetURL writes no file — the modeline points at URL.
	TargetURL
)

// Install renders + writes the schema (when applicable) and updates
// the yaml-language-server modeline in `<repoRoot>/.treeman.yaml`.
// Returns the resolved modeline target (path or URL) and whether
// the modeline was changed.
//
// TargetURL skips the file write — useful when the user wants
// editor hinting to follow upstream without committing a copy.
func Install(repoRoot string, t Target) (resolved string, modelineChanged bool, err error) {
	switch t {
	case TargetURL:
		resolved = URL
	case TargetGlobal:
		resolved, err = GlobalPath()
		if err != nil {
			return "", false, err
		}
	case TargetRepo:
		resolved = filepath.Join(repoRoot, "schemas", "treeman.schema.json")
	default:
		return "", false, fmt.Errorf("schema: invalid target %d", t)
	}
	if t != TargetURL {
		body, err := Render()
		if err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
			return "", false, err
		}
		if err := os.WriteFile(resolved, body, 0o644); err != nil {
			return "", false, err
		}
	}
	modelineChanged, err = SetModeline(repoRoot, resolved)
	return resolved, modelineChanged, err
}

// ReadModeline returns the `$schema=` target declared by the first
// `# yaml-language-server: ...` comment in `<repoRoot>/.treeman.yaml`,
// or "" when the file or modeline is absent.
func ReadModeline(repoRoot string) string {
	b, err := os.ReadFile(filepath.Join(repoRoot, ".treeman.yaml"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "#") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(trim, "#"))
		if !strings.HasPrefix(body, "yaml-language-server:") {
			continue
		}
		for _, tok := range strings.Fields(strings.TrimPrefix(body, "yaml-language-server:")) {
			if v, ok := strings.CutPrefix(tok, "$schema="); ok {
				return v
			}
		}
	}
	return ""
}

// SetModeline ensures the yaml-language-server modeline at the top
// of `<repoRoot>/.treeman.yaml` points at `target`. Returns
// (changed, err). When `.treeman.yaml` is missing the call is a
// no-op — `treeman init` is responsible for the initial scaffold.
func SetModeline(repoRoot, target string) (bool, error) {
	p := filepath.Join(repoRoot, ".treeman.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	newLine := "# yaml-language-server: $schema=" + target
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "# yaml-language-server:") {
			if line == newLine {
				return false, nil
			}
			lines[i] = newLine
			return true, os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o644)
		}
	}
	out := newLine + "\n" + string(raw)
	return true, os.WriteFile(p, []byte(out), 0o644)
}
