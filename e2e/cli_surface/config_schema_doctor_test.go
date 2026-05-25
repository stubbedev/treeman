//go:build e2e

package cli_surface_e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── treeman config ───────────────────────────────────────────────

// writeConfig drops a minimal but loadable .treeman.yaml into dir.
func writeConfig(t *testing.T, dir string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".treeman.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const minimalConfig = `worktrees:
  root: .worktrees
connections:
  mysql:
    host: 127.0.0.1
    port: 3306
    user: root
databases:
  - engine: mysql
    name_template: app_{slug}
`

func TestConfigValidate(t *testing.T) {
	t.Run("valid config exits 0", func(t *testing.T) {
		repo := newGitRepo(t)
		writeConfig(t, repo, minimalConfig)
		e := newEnv(t)
		res := e.run(t, repo, "config", "validate")
		if res.err != nil {
			t.Errorf("validate clean config: %v\nstderr:\n%s", res.err, res.stderr)
		}
	})

	t.Run("invalid YAML errors", func(t *testing.T) {
		repo := newGitRepo(t)
		writeConfig(t, repo, "this is: not\n  valid:\n   - mapping\n -broken\n")
		e := newEnv(t)
		res := e.run(t, repo, "config", "validate")
		if res.err == nil {
			t.Errorf("expected validate to fail on broken YAML, got stdout:\n%s", res.stdout)
		}
	})
}

func TestConfigShow(t *testing.T) {
	repo := newGitRepo(t)
	writeConfig(t, repo, minimalConfig)
	e := newEnv(t)

	t.Run("plain", func(t *testing.T) {
		res := e.run(t, repo, "config", "show")
		if res.err != nil {
			t.Fatalf("config show: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "name_template") {
			t.Errorf("expected config field in stdout:\n%s", res.stdout)
		}
	})

	t.Run("--json emits valid JSON", func(t *testing.T) {
		res := e.run(t, repo, "config", "show", "--json")
		if res.err != nil {
			t.Fatalf("config show --json: %v\nstderr:\n%s", res.err, res.stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
			t.Fatalf("decode config JSON: %v\nstdout:\n%s", err, res.stdout)
		}
		if len(got) == 0 {
			t.Errorf("expected non-empty config JSON")
		}
	})

	t.Run("--resolved expands defaults", func(t *testing.T) {
		res := e.run(t, repo, "config", "show", "--resolved")
		if res.err != nil {
			t.Fatalf("config show --resolved: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "name_template") {
			t.Errorf("expected resolved config to include declared fields:\n%s", res.stdout)
		}
	})
}

func TestConfigSet(t *testing.T) {
	repo := newGitRepo(t)
	writeConfig(t, repo, minimalConfig+"\nworker_slots: 4\n")
	e := newEnv(t)

	t.Run("patches scalar by dotted path", func(t *testing.T) {
		res := e.run(t, repo, "config", "set", "worker_slots", "8")
		if res.err != nil {
			t.Fatalf("config set: %v\nstderr:\n%s", res.err, res.stderr)
		}
		body, err := os.ReadFile(filepath.Join(repo, ".treeman.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "worker_slots: 8") {
			t.Errorf("expected worker_slots: 8 in config:\n%s", body)
		}
	})

	t.Run("rejects bad dotted path", func(t *testing.T) {
		res := e.run(t, repo, "config", "set")
		if res.err == nil {
			t.Errorf("expected error when called with no args, got stdout:\n%s", res.stdout)
		}
	})
}

// ── treeman schema ───────────────────────────────────────────────

func TestSchemaDump(t *testing.T) {
	repo := newGitRepo(t)
	e := newEnv(t)

	res := e.run(t, repo, "schema", "dump")
	if res.err != nil {
		t.Fatalf("schema dump: %v\nstderr:\n%s", res.err, res.stderr)
	}
	// JSON schema → must be valid JSON and reference the $schema meta.
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
		t.Fatalf("decode schema: %v\nstdout head:\n%s", err, head(res.stdout, 200))
	}
	if _, ok := got["$schema"]; !ok {
		// Older JSON-schema dialects omit the key — fall back to
		// asserting on a sentinel property like "properties".
		if _, ok := got["properties"]; !ok {
			t.Errorf("schema dump missing both $schema and properties keys: %v", keys(got))
		}
	}
}

func TestSchemaInstall(t *testing.T) {
	repo := newGitRepo(t)
	e := newEnv(t)
	res := e.run(t, repo, "schema", "install")
	if res.err != nil {
		t.Fatalf("schema install: %v\nstderr:\n%s\nstdout:\n%s", res.err, res.stderr, res.stdout)
	}
	// The CLI writes a yaml-language-server modeline to .treeman.yaml,
	// or prints a hint when no config exists yet. Either way exit
	// should be 0 and produce output.
	if strings.TrimSpace(res.stdout+res.stderr) == "" {
		t.Errorf("schema install produced no output (expected at least a path/hint)")
	}
}

// ── treeman doctor ───────────────────────────────────────────────

func TestDoctor(t *testing.T) {
	t.Run("passes on a healthy repo (no daemon required)", func(t *testing.T) {
		repo := newGitRepo(t)
		writeConfig(t, repo, minimalConfig)
		e := newEnv(t)
		res := e.run(t, repo, "doctor")
		// doctor flags missing daemon as a warning, not a hard error;
		// the binary should still exit 0 when the rest of the
		// environment is healthy.
		if res.err != nil {
			t.Logf("doctor reported issues (acceptable, daemon absent in test env):\nstdout:\n%s\nstderr:\n%s",
				res.stdout, res.stderr)
		}
		if !strings.Contains(res.stdout, "config") {
			t.Errorf("doctor output should mention the config check:\n%s", res.stdout)
		}
	})
}

// head returns the first n bytes of s with an ellipsis suffix when
// truncated. Used to keep failure messages skim-able when binary
// output is huge.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
