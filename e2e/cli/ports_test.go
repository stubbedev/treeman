//go:build e2e

// CLI-surface coverage for the dynamic-ports feature and the
// main-worktree delete guard. No engine needed — `wt create
// --skip-hooks` allocates ports and applies patches inline, and the
// delete guard fires before any daemon dispatch.
package cli_e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// setupPortsRepo builds the treeman binary, isolates state, and lays
// down a git repo whose .treeman.yaml declares an `octane` port slot
// and a patch that renders `{port_octane}` into .env.
func setupPortsRepo(t *testing.T) (binDir, repoRoot string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(filepath.Dir(root)) // up to project root
	binDir = t.TempDir()
	buildBin(t, binDir, root, "treeman", "./cmd/treeman")

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "treeman.db"))
	t.Setenv("TREEMAN_SOCKET", filepath.Join(t.TempDir(), "tm.sock"))

	repoRoot = filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repoRoot, "init", "-q", "-b", "main")
	mustGit(t, repoRoot, "config", "user.email", "e2e@example.com")
	mustGit(t, repoRoot, "config", "user.name", "e2e")

	write := func(rel, body string) {
		full := filepath.Join(repoRoot, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "OCTANE_PORT=0\n")
	write(".treeman.yaml", `
worktrees:
  root: .worktrees
  copies:
    - .env
ports:
  octane:
    range: [8200, 8299]
  webpack: [3200, 3299]
patches:
  - file: .env
    skip_worktree: false
    set:
      OCTANE_PORT: "{port_octane}"
      WEBPACK_PORT: "{port_webpack}"
`)
	mustGit(t, repoRoot, "add", "-A")
	mustGit(t, repoRoot, "commit", "-q", "-m", "init")
	return binDir, repoRoot
}

// TestPortsAllocateAndRenderAndShow drives the whole ports surface:
// allocation at `wt create`, the `{port_<name>}` patch token rendering
// into .env, and `wt show` (by name AND argument-free from inside the
// worktree) reporting the assignment.
func TestPortsAllocateAndRenderAndShow(t *testing.T) {
	binDir, repoRoot := setupPortsRepo(t)

	const branch = "octane-wt"
	out := runTreeman(t, binDir, repoRoot, "worktree", "create", branch, "--skip-hooks")
	t.Logf("wt create:\n%s", out)

	wtPath := filepath.Join(repoRoot, ".worktrees", branch)

	// The copied .env must have both tokens rendered to ports in range.
	envBody, err := os.ReadFile(filepath.Join(wtPath, ".env"))
	if err != nil {
		t.Fatalf("read patched .env: %v", err)
	}
	octane := portValue(t, string(envBody), "OCTANE_PORT")
	if octane < 8200 || octane > 8299 {
		t.Errorf("OCTANE_PORT=%d out of declared range [8200,8299]\n%s", octane, envBody)
	}
	webpack := portValue(t, string(envBody), "WEBPACK_PORT")
	if webpack < 3200 || webpack > 3299 {
		t.Errorf("WEBPACK_PORT=%d out of declared range [3200,3299]\n%s", webpack, envBody)
	}

	// `wt show <branch>` reports the ports.
	show := runTreeman(t, binDir, repoRoot, "worktree", "show", branch)
	if !strings.Contains(show, "octane="+strconv.Itoa(octane)) {
		t.Errorf("wt show missing octane port:\n%s", show)
	}

	// Argument-free `wt show` from inside the worktree resolves via cwd.
	showCwd := runTreeman(t, binDir, wtPath, "worktree", "show")
	if !strings.Contains(showCwd, "octane="+strconv.Itoa(octane)) {
		t.Errorf("cwd-aware wt show missing octane port:\n%s", showCwd)
	}
}

// TestDeleteRefusesMainWorktreeCLI guards against the data-loss footgun:
// `treeman wt delete .` at the repo root must be refused, even with
// --yes, before any teardown/DB drop runs.
func TestDeleteRefusesMainWorktreeCLI(t *testing.T) {
	binDir, repoRoot := setupPortsRepo(t)

	cmd := exec.Command(filepath.Join(binDir, "treeman"), "worktree", "delete", "--yes", ".")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("deleting the repo root must fail; got success:\n%s", out)
	}
	if !strings.Contains(string(out), "main worktree") {
		t.Errorf("error should mention the main worktree, got:\n%s", out)
	}
}

func portValue(t *testing.T, env, key string) int {
	t.Helper()
	m := regexp.MustCompile(key + `=(\d+)`).FindStringSubmatch(env)
	if m == nil {
		t.Fatalf("%s not found in .env:\n%s", key, env)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse %s=%q: %v", key, m[1], err)
	}
	return n
}
