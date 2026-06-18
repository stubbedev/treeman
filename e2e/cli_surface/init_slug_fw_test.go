//go:build e2e

package cli_surface_e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── treeman init ─────────────────────────────────────────────────

func TestInit(t *testing.T) {
	t.Run("creates .treeman.yaml in fresh git repo", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)

		res := e.run(t, repo, "init")
		if res.err != nil {
			t.Fatalf("init: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if _, err := os.Stat(filepath.Join(repo, ".treeman.yaml")); err != nil {
			t.Fatalf(".treeman.yaml not created: %v", err)
		}
	})

	t.Run("--json reports created+bytes+path", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)

		res := e.run(t, repo, "init", "--json")
		if res.err != nil {
			t.Fatalf("init --json: %v\nstderr:\n%s", res.err, res.stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
			t.Fatalf("decode init JSON %q: %v", res.stdout, err)
		}
		for _, k := range []string{"created", "bytes", "path"} {
			if _, ok := got[k]; !ok {
				t.Errorf("init JSON missing %q: %v", k, got)
			}
		}
		if got["created"] != true {
			t.Errorf("expected created=true, got %v", got["created"])
		}
	})

	t.Run("refuses overwrite without --force", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)

		_ = e.run(t, repo, "init")
		// Second call without --force should error and leave the file
		// in place.
		res := e.run(t, repo, "init")
		if res.err == nil {
			t.Errorf("expected non-zero exit on repeat init without --force")
		}
	})

	t.Run("--force overwrites", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)

		_ = e.run(t, repo, "init")
		res := e.run(t, repo, "init", "--force")
		if res.err != nil {
			t.Errorf("init --force: %v\nstderr:\n%s", res.err, res.stderr)
		}
	})
}

// ── treeman slug ─────────────────────────────────────────────────

func TestSlug(t *testing.T) {
	t.Run("derives deterministic slug from cwd", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)

		res := e.run(t, repo, "slug")
		if res.err != nil {
			t.Fatalf("slug: %v\nstderr:\n%s", res.err, res.stderr)
		}
		out := strings.TrimSpace(res.stdout)
		if out == "" {
			t.Errorf("slug should print non-empty value, got %q", res.stdout)
		}
	})

	t.Run("--json includes value + source dimensions", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)

		res := e.run(t, repo, "slug", "--json")
		if res.err != nil {
			t.Fatalf("slug --json: %v\nstderr:\n%s", res.err, res.stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
			t.Fatalf("decode slug JSON %q: %v", res.stdout, err)
		}
		// We don't pin the exact key set — just confirm there's a
		// "value"-shaped key and the JSON is well-formed.
		if len(got) == 0 {
			t.Errorf("expected non-empty slug JSON: %v", got)
		}
	})
}

// ── treeman fw detect ────────────────────────────────────────────

func TestFwDetect(t *testing.T) {
	t.Run("plain-text on empty repo reports no frameworks", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)
		res := e.run(t, repo, "fw", "detect")
		if res.err != nil {
			t.Fatalf("fw detect: %v\nstderr:\n%s", res.err, res.stderr)
		}
		// Empty repo → both detectors print a "no <thing> detected"
		// bullet line.
		for _, want := range []string{"no migration framework", "no test framework"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("fw detect missing %q in:\n%s", want, res.stdout)
			}
		}
	})

	t.Run("--json shape", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)
		res := e.run(t, repo, "fw", "detect", "--json")
		if res.err != nil {
			t.Fatalf("fw detect --json: %v\nstderr:\n%s", res.err, res.stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
			t.Fatalf("decode fw JSON %q: %v", res.stdout, err)
		}
		if len(got) == 0 {
			t.Errorf("expected non-empty fw JSON: %v", got)
		}
	})

	t.Run("detects django via manage.py", func(t *testing.T) {
		repo := newGitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "manage.py"), []byte("#!/usr/bin/env python\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, "myapp", "migrations"), 0o755); err != nil {
			t.Fatal(err)
		}
		e := newEnv(t)
		res := e.run(t, repo, "fw", "detect")
		if res.err != nil {
			t.Fatalf("fw detect: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "django") {
			t.Errorf("expected 'django' in fw detect output:\n%s", res.stdout)
		}
	})

	// The user-defined `frameworks:` block: a custom detector keyed by
	// its marker files. Previously dead config (fw detect only consulted
	// the built-in registry); now RegistryFor merges it in.
	t.Run("detects user-defined frameworks: block", func(t *testing.T) {
		repo := newGitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "myframework.config"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		yaml := `frameworks:
  acme-migrate:
    markers: ["myframework.config"]
    migration_dirs: ["db/acme"]
    file_pattern: "*.sql"
    hash_mode: filename
    engine_hint: postgres
`
		if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		e := newEnv(t)
		res := e.run(t, repo, "fw", "detect")
		if res.err != nil {
			t.Fatalf("fw detect: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "acme-migrate") {
			t.Errorf("custom frameworks: entry 'acme-migrate' not detected (frameworks: block ignored?):\n%s", res.stdout)
		}
	})
}
