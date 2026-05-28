//go:build e2e

// Package hookcwd_e2e exercises the hook Action `cwd:` field — the
// per-action working directory. The doc contract is "relative paths
// resolve against the worktree root"; this asserts a hook with
// `cwd: <subdir>` actually runs its command in <worktree>/<subdir>.
// No docker: the hook fires in a goroutine off FinalizeWorktree, and
// with no databases declared prepare short-circuits.
package hookcwd_e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

func TestHookActionCwd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repoRoot := t.TempDir()
	witnessDir := filepath.Join(repoRoot, "touch")
	mustMkdir(t, witnessDir)
	// The subdir the hook should run in.
	mustMkdir(t, filepath.Join(repoRoot, "backend", "app"))

	mustGit(t, "", "init", "-q", "-b", "main", repoRoot)
	mustGit(t, repoRoot, "config", "user.email", "t@t")
	mustGit(t, repoRoot, "config", "user.name", "t")
	writeFile(t, repoRoot, "README", "hi")
	// `pwd` runs in the action's cwd; write it to an absolute witness path.
	yaml := `
main_worktree:
  enabled: true
hooks:
  on-create-before-engines:
    - cwd: backend/app
      run: pwd > ` + filepath.Join(witnessDir, "pwd") + `
`
	writeFile(t, repoRoot, ".treeman.yaml", yaml)
	mustGit(t, repoRoot, "add", "-A")
	mustGit(t, repoRoot, "commit", "-q", "-m", "init")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	state := daemon.NewState(ctx, st)

	if _, err := daemon.EnrollMainWorktree(ctx, state, repoRoot); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, map[string]string{"PATH": os.Getenv("PATH")}); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	got := strings.TrimSpace(waitForFile(t, filepath.Join(witnessDir, "pwd"), 15*time.Second))
	// Resolve symlinks both sides (macOS /tmp → /private/tmp, etc.).
	wantAbs, _ := filepath.EvalSymlinks(filepath.Join(repoRoot, "backend", "app"))
	gotAbs, _ := filepath.EvalSymlinks(got)
	if gotAbs != wantAbs {
		t.Fatalf("hook cwd ran in %q, want %q (relative cwd must resolve against the worktree root)", gotAbs, wantAbs)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
			return string(body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared within %s", path, timeout)
	return ""
}
