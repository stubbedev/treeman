package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckHooksBootScriptGuard covers the three verdicts issue #27 asks
// for: an unguarded install in create-before-engines warns, the same
// install with the scripts disarmed (flag or env guard) is ok, and a repo
// whose manifests boot nothing is ok regardless of the hook.
func TestCheckHooksBootScriptGuard(t *testing.T) {
	const composerBoots = `{"scripts":{"post-autoload-dump":["@php artisan package:discover --ansi"]}}`
	const composerInert = `{"scripts":{"post-autoload-dump":["echo hi"]}}`

	cases := []struct {
		name     string
		composer string
		hook     string
		want     string
	}{
		{"unguarded install warns", composerBoots, "composer install --no-interaction", "warn"},
		{"no-scripts is fine", composerBoots, "composer install --no-scripts", "ok"},
		{"env guard is fine", composerBoots, "DB_DATABASE=none composer install", "ok"},
		{"inert boot script is fine", composerInert, "composer install", "ok"},
		{"unrelated step is fine", composerBoots, "git pull --ff-only", "ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := t.TempDir()
			write := func(name, body string) {
				if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("composer.json", c.composer)
			write(".treeman.yaml", "hooks:\n  create-before-engines:\n    - run: \""+c.hook+"\"\n")
			if got := checkHooks(repo); got.Status != c.want {
				t.Errorf("status = %q, want %q (detail: %s)", got.Status, c.want, got.Detail)
			}
		})
	}
}

// A repo with no create-before-engines hooks has nothing to check.
func TestCheckHooksNoBeforeEnginesSkips(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte("databases: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := checkHooks(repo); got.Status != "skip" {
		t.Errorf("status = %q, want skip (detail: %s)", got.Status, got.Detail)
	}
}
