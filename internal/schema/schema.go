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

// URL is the canonical upstream URL for the REPO-scoped schema that
// validates a per-repo `.treeman.yaml`. Used by `treeman init`
// (modeline default) and the `--url` install mode. CI publishes the
// repo-scoped schema at this path (see .github/workflows/ci.yml).
const URL = "https://raw.githubusercontent.com/stubbedev/treeman/master/schemas/treeman.schema.json"

// GlobalURL is the canonical upstream URL for the GLOBAL-scoped schema
// that validates `~/.config/treeman/config.yaml`. CI publishes it
// alongside the repo schema so editors can validate the global config
// without a local `schema install --global`.
const GlobalURL = "https://raw.githubusercontent.com/stubbedev/treeman/master/schemas/treeman.global.schema.json"

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

// Scope selects which subset of the config a generated schema covers.
// `.treeman.yaml` (repo) and `~/.config/treeman/config.yaml` (global)
// share one Go struct but accept different key subsets; the scope
// filters the reflected schema down to the keys valid for one layer.
type Scope string

const (
	// ScopeFull keeps every key — the canonical union schema (the
	// checked-in `schemas/treeman.schema.json` and the upstream URL).
	ScopeFull Scope = "full"
	// ScopeGlobal keeps keys tagged `scope:"global"` or `"both"` —
	// the keys meaningful in the user-global config.
	ScopeGlobal Scope = "global"
	// ScopeRepo keeps keys tagged `scope:"repo"` or `"both"` — the
	// keys meaningful in a per-repo `.treeman.yaml`.
	ScopeRepo Scope = "repo"
)

// keepForScope reports whether a field with the given `scope:` tag
// value belongs in a schema generated for `want`. "both" is always
// kept; ScopeFull keeps everything.
func keepForScope(fieldScope string, want Scope) bool {
	if want == ScopeFull || fieldScope == "both" {
		return true
	}
	return Scope(fieldScope) == want
}

// ReflectScoped returns the schema for config.Config restricted to the
// top-level keys valid for `scope`. ScopeFull is identical to Reflect.
// Filtering happens on the reflected schema's root properties using the
// `scope:` struct tags exposed by config.FieldScopes — so the global
// schema drops repo-only keys (databases, patches, …) and the repo
// schema drops global-only keys (daemon, snapshots, logs, …).
func ReflectScoped(scope Scope) *jsonschema.Schema {
	s := Reflect()
	if scope == ScopeFull || s.Properties == nil {
		return s
	}
	scopes := config.FieldScopes()
	// Collect first — deleting during KeysFromOldest mutates the
	// underlying linked list and aborts the iteration early.
	var drop []string
	for key := range s.Properties.KeysFromOldest() {
		fs := scopes[key]
		if fs == "" {
			fs = "both"
		}
		if !keepForScope(fs, scope) {
			drop = append(drop, key)
		}
	}
	for _, key := range drop {
		s.Properties.Delete(key)
	}
	return s
}

// RenderScoped renders ReflectScoped(scope) as pretty-printed bytes.
func RenderScoped(scope Scope) ([]byte, error) {
	return json.MarshalIndent(ReflectScoped(scope), "", "  ")
}

// GlobalPath returns the user-global schema path as a sibling of the
// global config.yaml (`$XDG_CONFIG_HOME/treeman/treeman.schema.json`,
// falling back to `~/.config/treeman/...`). Derived from
// config.GlobalConfigPath so the schema always lands beside the config
// it validates — os.UserConfigDir would diverge on macOS, where it
// ignores XDG_CONFIG_HOME and returns ~/Library/Application Support.
func GlobalPath() (string, error) {
	cfgPath, ok := config.GlobalConfigPath()
	if !ok {
		return "", errors.New("cannot resolve user-global config dir")
	}
	return filepath.Join(filepath.Dir(cfgPath), "treeman.schema.json"), nil
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
		// Each config layer accepts a different key subset, so each gets
		// its scope-filtered schema: the global config drops repo-only
		// keys, the repo `.treeman.yaml` drops global-only keys. This is
		// what makes the scope split a hard break in editors too — a
		// daemon: block in a repo file shows as an unknown key.
		scope := ScopeRepo
		if t == TargetGlobal {
			scope = ScopeGlobal
		}
		body, err := RenderScoped(scope)
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
	// The modeline goes in the file that the installed schema validates:
	// the global config for TargetGlobal, the repo `.treeman.yaml`
	// otherwise. Without this split, `schema install --global` would
	// point the repo file at the (narrower) global schema and break
	// repo-only key validation.
	modelineFile := filepath.Join(repoRoot, ".treeman.yaml")
	if t == TargetGlobal {
		gp, ok := config.GlobalConfigPath()
		if !ok {
			return resolved, false, nil
		}
		modelineFile = gp
	}
	modelineChanged, err = setModelineAt(modelineFile, resolved)
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
	return setModelineAt(filepath.Join(repoRoot, ".treeman.yaml"), target)
}

// setModelineAt is SetModeline against an explicit file path so the
// global-config install can target `~/.config/treeman/config.yaml`
// instead of a repo file.
func setModelineAt(p, target string) (bool, error) {
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
