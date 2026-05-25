// Apply orchestrates one top-level `patches:` entry: pick the driver
// (explicit `format:` or extension auto-detect), render every value
// template through the template package, dispatch, and apply
// skip-worktree.
package patcher

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/template"
)

// ApplyResult is the per-file outcome of one Patch application.
type ApplyResult struct {
	File    string  // relative path from the YAML
	Outcome Outcome // Updated / Unchanged / Missing
	Driver  string  // dotenv | phpunit | yaml | json | toml | ini
	Skipped bool    // true when SkipWorktree applied + git was happy
}

// Apply executes one Patch against the given worktree. Returns
// Missing+nil error when the target file isn't present (caller may
// log + continue). Returns an error only on bad config or write
// failure.
func Apply(p config.Patch, worktreePath string, tplCtx template.Context) (ApplyResult, error) {
	if len(p.Set) == 0 {
		return ApplyResult{}, fmt.Errorf("patch %s: `set:` is empty", p.File)
	}
	driver := strings.ToLower(strings.TrimSpace(p.Format))
	if driver == "" {
		driver = detectFormat(p.File)
	}
	if driver == "" {
		return ApplyResult{}, fmt.Errorf("patch %s: cannot infer format from extension; set `format:` explicitly", p.File)
	}
	rendered, err := renderTemplates(p.File, p.Set, tplCtx)
	if err != nil {
		return ApplyResult{}, err
	}
	abs := filepath.Join(worktreePath, p.File)
	var outcome Outcome
	switch driver {
	case "dotenv":
		outcome, err = PatchEnvFile(abs, pairsFrom(rendered))
	case "phpunit", "phpunit_env":
		outcome, err = PatchPhpunitFile(abs, pairsFrom(rendered))
	case "yaml":
		outcome, err = PatchYAMLFile(abs, rendered)
	case "json":
		outcome, err = PatchJSONFile(abs, rendered)
	case "toml":
		outcome, err = PatchTOMLFile(abs, rendered)
	case "ini":
		outcome, err = PatchINIFile(abs, rendered)
	default:
		return ApplyResult{}, fmt.Errorf("patch %s: unknown format %q (want dotenv|phpunit|yaml|json|toml|ini)", p.File, driver)
	}
	if err != nil {
		return ApplyResult{}, fmt.Errorf("patch %s: %w", p.File, err)
	}
	res := ApplyResult{File: p.File, Outcome: outcome, Driver: driver}

	skip := p.SkipWorktree == nil || *p.SkipWorktree
	if skip && outcome != Missing {
		if ok, _ := SkipWorktree(worktreePath, abs); ok {
			res.Skipped = true
		}
	}
	return res, nil
}

// detectFormat picks a driver from a file path. Returns "" when the
// extension is unknown so the caller can demand explicit `format:`.
func detectFormat(file string) string {
	base := strings.ToLower(filepath.Base(file))
	// `.env`, `.env.local`, `.env.testing.local` — all dotenv. Match
	// on prefix so `.env-flavour` variants line up.
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		return "dotenv"
	}
	ext := filepath.Ext(base)
	// Strip `.dist` from e.g. `phpunit.xml.dist` so the real format
	// extension (.xml) is what we match on.
	if ext == ".dist" {
		ext = filepath.Ext(strings.TrimSuffix(base, ".dist"))
	}
	switch ext {
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	case ".toml":
		return "toml"
	case ".ini", ".cfg":
		return "ini"
	case ".xml":
		// Default XML driver is phpunit since that's the only XML
		// shape we know how to edit. Users with other XML files
		// should set `format:` explicitly (and we'll error since
		// we don't have a generic XML driver).
		return "phpunit"
	}
	return ""
}

func renderTemplates(file string, raw map[string]string, tplCtx template.Context) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, err := template.Render(v, tplCtx)
		if err != nil {
			return nil, fmt.Errorf("patch %s: render value for %q: %w", file, k, err)
		}
		out[k] = s
	}
	return out, nil
}

func pairsFrom(m map[string]string) []Pair {
	out := make([]Pair, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, Pair{Key: k, Value: m[k]})
	}
	return out
}
