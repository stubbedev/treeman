package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/stubbedev/treeman/internal/resolve"
)

// checkHooks probes one thing: a `create-before-engines` hook that
// installs dependencies with the framework's boot scripts still armed.
//
// That phase runs before the engine databases exist, so a package
// manager whose post-install script boots the app (Laravel's
// `post-autoload-dump` → `artisan package:discover` / `vendor:publish`,
// an npm `postinstall` that runs the framework CLI) queries a database
// that is not there yet and dies with "Unknown database". The failure is
// non-deterministic — it only lands when the boot happens to touch the
// DB (issue #27: a `random_int(1, 50)` prune gate in a Telescope
// listener, so 2 of 9 concurrent creates failed) — and it costs the
// whole cold install first, 39 s to 164 s, before surfacing.
//
// The contract is documented, but nothing enforced it, so a repo could
// carry the unguarded hook for weeks. This turns it into a warn at
// config-load time.
func checkHooks(repoRoot string) doctorResult {
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return doctorResult{Name: "hooks", Status: "skip", Detail: "config not loadable: " + err.Error()}
	}
	before := cfg.Hooks.OnCreateBeforeEngines
	if len(before) == 0 {
		return doctorResult{Name: "hooks", Status: "skip", Detail: "no create-before-engines hooks"}
	}
	bootScripts := repoBootScripts(repoRoot)
	if len(bootScripts) == 0 {
		return doctorResult{
			Name:   "hooks",
			Status: "ok",
			Detail: fmt.Sprintf("%d create-before-engines action(s); no framework boot scripts in the repo's manifests", len(before)),
		}
	}

	var risky []string
	for _, a := range before {
		for _, step := range a.Run {
			if mgr := bootingPackageManager(step); mgr != "" && bootScripts[mgr] != "" && !scriptsDisarmed(step, mgr) {
				risky = append(risky, fmt.Sprintf("%q (%s runs %s)", step, mgr, bootScripts[mgr]))
			}
		}
	}
	if len(risky) == 0 {
		return doctorResult{
			Name:   "hooks",
			Status: "ok",
			Detail: fmt.Sprintf("%d create-before-engines action(s); no unguarded dependency install", len(before)),
		}
	}
	return doctorResult{
		Name:   "hooks",
		Status: "warn",
		Detail: "create-before-engines installs dependencies with framework boot scripts armed, but the engine databases do not exist in that phase: " +
			strings.Join(
				risky,
				"; ",
			),
		Hint: "disarm the scripts in this phase (`composer install --no-scripts` / `npm install --ignore-scripts`) and re-run them from " +
			"create-after-engines, or move the whole install there — see the Laravel recipe in docs/configuration.md#hooks",
	}
}

// reEnvGuard matches an inline env assignment at the start of the step
// or of a chained segment — `DB_DATABASE=… composer install`, the guard
// shape issue #27 asks to be treated as deliberate.
var reEnvGuard = regexp.MustCompile(`(^|&&\s*|;\s*|\|\|\s*)[A-Za-z_][A-Za-z0-9_]*=`)

// bootingPackageManager classifies a hook step as a dependency install
// whose post-install scripts can boot the app. Returns the manifest key
// ("composer" / "npm") the scripts would come from, or "".
func bootingPackageManager(step string) string {
	s := strings.ToLower(step)
	switch {
	case strings.Contains(s, "composer install"), strings.Contains(s, "composer update"),
		strings.Contains(s, "composer dump-autoload"):
		return "composer"
	case strings.Contains(s, "npm install"), strings.Contains(s, "npm ci"),
		strings.Contains(s, "yarn install"), strings.Contains(s, "pnpm install"),
		strings.Contains(s, "bun install"):
		return "npm"
	}
	return ""
}

// scriptsDisarmed reports whether the step already opts out of the
// post-install scripts, or prefixes an env guard.
func scriptsDisarmed(step, mgr string) bool {
	s := strings.ToLower(step)
	flag := "--no-scripts"
	if mgr == "npm" {
		flag = "--ignore-scripts"
	}
	return strings.Contains(s, flag) || reEnvGuard.MatchString(step)
}

// repoBootScripts reads composer.json / package.json and reports, per
// manifest key, the install script that boots the framework — the thing
// that turns a dependency install into an app boot. Returns only the
// entries whose script actually invokes a framework CLI; a manifest
// whose post-install hook just moves files is not a problem here.
func repoBootScripts(repoRoot string) map[string]string {
	out := map[string]string{}
	type manifest struct {
		file    string
		key     string
		scripts []string
	}
	for _, m := range []manifest{
		{"composer.json", "composer", []string{"post-autoload-dump", "post-install-cmd", "post-update-cmd"}},
		{"package.json", "npm", []string{"postinstall", "prepare"}},
	} {
		body, err := os.ReadFile(filepath.Join(repoRoot, m.file))
		if err != nil {
			continue
		}
		var doc struct {
			Scripts map[string]json.RawMessage `json:"scripts"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			continue
		}
		for _, name := range m.scripts {
			raw, ok := doc.Scripts[name]
			if !ok {
				continue
			}
			if cmd := frameworkCommandIn(string(raw)); cmd != "" {
				out[m.key] = name + " → " + cmd
				break
			}
		}
	}
	return out
}

// frameworkCommandIn returns the first framework CLI referenced in a
// raw script value (string or array of strings, hence the substring
// scan over the raw JSON), or "".
func frameworkCommandIn(raw string) string {
	for _, cli := range []string{"artisan", "bin/console", "bin/rails", "manage.py", "craft", "yii"} {
		if strings.Contains(raw, cli) {
			return cli
		}
	}
	return ""
}
