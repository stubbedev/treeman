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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/stubbedev/treeman/internal/config"
)

// URL is the canonical upstream URL for the generated schema. Used
// by `treeman init` (modeline default) and the `--url` install mode.
const URL = "https://raw.githubusercontent.com/stubbedev/treeman/master/schemas/treeman.schema.json"

// Reflect returns the live *jsonschema.Schema for config.Config,
// generated via reflection over the Go types + their doc comments.
// Single source of truth for both the JSON Schema (Render) and the
// generated markdown config reference (cmd/treeman-gen-config-docs),
// so neither can drift from the structs.
func Reflect() *jsonschema.Schema {
	r := &jsonschema.Reflector{
		Anonymous:      true,
		ExpandedStruct: true,
		FieldNameTag:   "yaml",
		CommentMap:     config.CommentMap(),
	}
	return r.Reflect(&config.Config{})
}

// Render returns the JSON Schema for config.Config as pretty-printed
// bytes. Generated via reflection so adding a Go field automatically
// flows into editor hinting.
func Render() ([]byte, error) {
	return json.MarshalIndent(Reflect(), "", "  ")
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

// ProbeRef resolves a yaml-language-server modeline target (URL or
// path) and reports whether it's reachable. Both http(s) URLs and
// file paths are accepted; `file://` URLs are normalised to their
// underlying path before stat. http(s) URLs are HEAD-checked with a
// short timeout so doctor doesn't hang offline.
//
// Returns (true, <resolved-detail>) when the target resolves, or
// (false, <reason>) when it doesn't. `repoRoot` is used to resolve
// relative file paths; pass "" if the ref is already absolute.
func ProbeRef(repoRoot, ref string) (bool, string) {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		client := &http.Client{Timeout: 3 * time.Second}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, ref, nil)
		if err != nil {
			return false, fmt.Sprintf("invalid URL: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, fmt.Sprintf("unreachable: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return true, ref
		}
		return false, fmt.Sprintf("HTTP %d at %s", resp.StatusCode, ref)
	}
	p := ref
	if strings.HasPrefix(ref, "file://") {
		u, err := url.Parse(ref)
		if err != nil {
			return false, fmt.Sprintf("invalid file URL: %v", err)
		}
		p = u.Path
	}
	if !filepath.IsAbs(p) && repoRoot != "" {
		p = filepath.Join(repoRoot, p)
	}
	if _, err := os.Stat(p); err == nil {
		return true, p
	}
	return false, "missing file: " + p
}

// ReadModeline returns the `$schema=` target declared by the first
// `# yaml-language-server: ...` comment in `<repoRoot>/.treeman.yaml`,
// or "" when the file or modeline is absent.
func ReadModeline(repoRoot string) string {
	b, err := os.ReadFile(filepath.Join(repoRoot, ".treeman.yaml"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "#") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(trim, "#"))
		if !strings.HasPrefix(body, "yaml-language-server:") {
			continue
		}
		for tok := range strings.FieldsSeq(strings.TrimPrefix(body, "yaml-language-server:")) {
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
