//go:build e2e

// Package patches_e2e exercises the full Patch pipeline (config →
// patcher.Apply) across every supported driver: dotenv, phpunit,
// yaml, json, toml, ini. Each subtest writes a fixture, runs Apply
// through the daemon path, and re-reads the file to confirm the
// rewrite landed AND that `{slug}` was substituted.
//
// Also exercises auto-detection from extension and the explicit
// `format:` override.
package patches_e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/patcher"
	"github.com/stubbedev/treeman/internal/template"
)

func TestPatchesEveryDriver(t *testing.T) {
	tplCtx := template.Context{
		Slug:     "wt_alpha",
		SlugDash: "wt-alpha",
	}

	cases := []struct {
		name     string
		filename string
		initial  string
		patch    config.Patch
		assert   func(t *testing.T, body string)
	}{
		{
			name:     "dotenv",
			filename: ".env",
			initial:  "DB_HOST=127.0.0.1\nDB_DATABASE=old_value\n",
			patch: config.Patch{
				File: ".env",
				Set:  map[string]string{"DB_DATABASE": "app_{slug}"},
			},
			assert: func(t *testing.T, body string) {
				if !strings.Contains(body, "DB_DATABASE=app_wt_alpha") {
					t.Errorf("dotenv missing rewrite: %q", body)
				}
				if !strings.Contains(body, "DB_HOST=127.0.0.1") {
					t.Errorf("dotenv lost unrelated key: %q", body)
				}
			},
		},
		{
			name:     "phpunit",
			filename: "phpunit.xml",
			initial: `<?xml version="1.0"?>
<phpunit>
  <php>
    <env name="DB_DATABASE" value="old"/>
    <env name="OTHER" value="kept"/>
  </php>
</phpunit>
`,
			patch: config.Patch{
				File:   "phpunit.xml",
				Format: "phpunit",
				Set:    map[string]string{"DB_DATABASE": "app_{slug}_test"},
			},
			assert: func(t *testing.T, body string) {
				if !strings.Contains(body, `value="app_wt_alpha_test"`) {
					t.Errorf("phpunit missing rewrite: %q", body)
				}
				if !strings.Contains(body, `name="OTHER" value="kept"`) {
					t.Errorf("phpunit lost unrelated env: %q", body)
				}
			},
		},
		{
			name:     "yaml",
			filename: "config.yaml",
			initial: `database:
  name: old
  host: 127.0.0.1
`,
			patch: config.Patch{
				File: "config.yaml",
				Set:  map[string]string{"database.name": "yaml_{slug}"},
			},
			assert: func(t *testing.T, body string) {
				if !strings.Contains(body, "name: yaml_wt_alpha") {
					t.Errorf("yaml missing rewrite: %q", body)
				}
				if !strings.Contains(body, "host: 127.0.0.1") {
					t.Errorf("yaml lost unrelated key: %q", body)
				}
			},
		},
		{
			name:     "json",
			filename: "config.json",
			initial:  `{"database":{"name":"old","host":"127.0.0.1"}}`,
			patch: config.Patch{
				File: "config.json",
				Set:  map[string]string{"database.name": "json_{slug}"},
			},
			assert: func(t *testing.T, body string) {
				if !strings.Contains(body, `"name": "json_wt_alpha"`) &&
					!strings.Contains(body, `"name":"json_wt_alpha"`) {
					t.Errorf("json missing rewrite: %q", body)
				}
				if !strings.Contains(body, `"host"`) {
					t.Errorf("json lost unrelated key: %q", body)
				}
			},
		},
		{
			name:     "toml",
			filename: "config.toml",
			initial: `[database]
name = "old"
host = "127.0.0.1"
`,
			patch: config.Patch{
				File: "config.toml",
				Set:  map[string]string{"database.name": "toml_{slug}"},
			},
			assert: func(t *testing.T, body string) {
				if !strings.Contains(body, "toml_wt_alpha") {
					t.Errorf("toml missing rewrite: %q", body)
				}
				if !strings.Contains(body, "127.0.0.1") {
					t.Errorf("toml lost unrelated key: %q", body)
				}
			},
		},
		{
			name:     "ini",
			filename: "config.ini",
			initial: `[database]
name=old
host=127.0.0.1
`,
			patch: config.Patch{
				File: "config.ini",
				Set:  map[string]string{"database.name": "ini_{slug}"},
			},
			assert: func(t *testing.T, body string) {
				if !strings.Contains(body, "ini_wt_alpha") {
					t.Errorf("ini missing rewrite: %q", body)
				}
				if !strings.Contains(body, "127.0.0.1") {
					t.Errorf("ini lost unrelated key: %q", body)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wt := t.TempDir()
			full := filepath.Join(wt, c.filename)
			if err := os.WriteFile(full, []byte(c.initial), 0o644); err != nil {
				t.Fatal(err)
			}

			res, err := patcher.Apply(c.patch, wt, tplCtx)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			t.Logf("%s: driver=%s outcome=%v skipped=%t", c.name,
				res.Driver, res.Outcome, res.Skipped)

			body, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			c.assert(t, string(body))
		})
	}
}
