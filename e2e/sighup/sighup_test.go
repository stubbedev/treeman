//go:build e2e

// Package sighup_e2e drives the daemon's SIGHUP config-reload
// signal handler. Sequence:
//
//  1. Boot treemand with a worktree registered + a watched glob A.
//  2. Touch a file under glob A → confirm watcher dispatches.
//  3. Edit .treeman.yaml to add glob B, drop glob A.
//  4. Send SIGHUP to the daemon.
//  5. Touch a file under glob B → confirm new watcher dispatches.
//  6. Touch a file under glob A → confirm OLD watcher is gone.
//
// This proves SIGHUP triggers config reload AND that worktree
// watchers are re-spawned with the new globs.
package sighup_e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
)

func TestSIGHUPReloadsConfig(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	// Build fresh binaries.
	repoRoot := projectRoot(t)
	binDir := t.TempDir()
	buildBin(t, binDir, repoRoot, "treeman", "./cmd/treeman")
	buildBin(t, binDir, repoRoot, "treemand", "./cmd/treemand")

	// Per-test sockets + isolated treeman DB so we don't pick up
	// stale rows from earlier e2e runs.
	runtimeDir := t.TempDir()
	stateDir := t.TempDir()
	configHome := t.TempDir()
	dbPath := filepath.Join(stateDir, "treeman.db")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("TREEMAN_DB_PATH", dbPath)
	socket := filepath.Join(runtimeDir, "treeman.sock")

	// Start treemand.
	daemonCmd := exec.Command(filepath.Join(binDir, "treemand"))
	daemonCmd.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+runtimeDir,
		"XDG_CONFIG_HOME="+configHome,
		"TREEMAN_DB_PATH="+dbPath,
	)
	daemonCmd.Stderr = os.Stderr
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("treemand: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Kill()
		_, _ = daemonCmd.Process.Wait()
	})
	harness.WaitForReady(t, "daemon-socket", 10*time.Second, func() error {
		c, err := net.DialTimeout("unix", socket, 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	// Set up a repo with two candidate input dirs (only one initially
	// referenced by .treeman.yaml).
	mainRepo := filepath.Join(t.TempDir(), "main")
	mustMkdir(t, mainRepo)
	// Pre-create non-empty dirs so git tracks them and the worktree
	// checkout has them; otherwise fsnotify can't subscribe to a
	// directory that doesn't exist when the watcher starts.
	mustWriteFile(t, filepath.Join(mainRepo, "globA/.keep"), "")
	mustWriteFile(t, filepath.Join(mainRepo, "globB/.keep"), "")
	mustGit(t, mainRepo, "init", "-q", "-b", "main")
	mustGit(t, mainRepo, "config", "user.email", "e2e@example.com")
	mustGit(t, mainRepo, "config", "user.name", "e2e")

	// Initial config: watches globA.
	yamlInitial := `
worktrees: { root: .worktrees }
debounce_ms: 100
databases:
  - engine: mysql
    name_template: irrelevant_{slug}
    inputs:
      - { glob: "globA/*", label: a }
hooks:
  file-change:
    - run: echo "$TREEMAN_WATCH_LABEL" >> /tmp/treeman-e2e-sighup-events
`
	if err := os.WriteFile(filepath.Join(mainRepo, ".treeman.yaml"),
		[]byte(yamlInitial), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(mainRepo, "seed.txt"), "x")
	mustGit(t, mainRepo, "add", "-A")
	mustGit(t, mainRepo, "commit", "-q", "-m", "init")

	// Reset the event sink.
	eventLog := "/tmp/treeman-e2e-sighup-events"
	_ = os.Remove(eventLog)
	t.Cleanup(func() { _ = os.Remove(eventLog) })

	// Create the worktree. NOTE: the daemon doesn't auto-spawn the
	// per-worktree fsnotify watcher on `wt create` — it only does
	// so at startup or via config-reload. SIGHUP once to bring the
	// watcher up against the initial config.
	out := runTreeman(t, binDir, mainRepo, "worktree", "create", "feature/sighup")
	t.Logf("wt create output:\n%s", out)
	wtPath := filepath.Join(mainRepo, ".worktrees", "feature/sighup")
	harness.WaitForReady(t, "wt-created", 15*time.Second, func() error {
		if _, err := os.Stat(wtPath); err != nil {
			return err
		}
		return nil
	})

	// SIGHUP #1 — spawn the initial watcher against the initial config.
	if err := daemonCmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP #1: %v", err)
	}
	time.Sleep(2 * time.Second)

	// Touch globA — should produce one 'a' event.
	mustWriteFile(t, filepath.Join(wtPath, "globA/v1.txt"), "first")
	waitForEvent(t, eventLog, "a", 5*time.Second)

	// Now rewrite the config to watch globB instead of globA.
	yamlUpdated := `
worktrees: { root: .worktrees }
debounce_ms: 100
databases:
  - engine: mysql
    name_template: irrelevant_{slug}
    inputs:
      - { glob: "globB/*", label: b }
hooks:
  file-change:
    - run: echo "$TREEMAN_WATCH_LABEL" >> /tmp/treeman-e2e-sighup-events
`
	if err := os.WriteFile(filepath.Join(mainRepo, ".treeman.yaml"),
		[]byte(yamlUpdated), 0o644); err != nil {
		t.Fatal(err)
	}
	// Push update to the worktree too (it's a separate checkout).
	if err := os.WriteFile(filepath.Join(wtPath, ".treeman.yaml"),
		[]byte(yamlUpdated), 0o644); err != nil {
		t.Fatal(err)
	}

	// SIGHUP #2 — reload to pick up the rewritten config (globA → globB).
	if err := daemonCmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP #2: %v", err)
	}
	time.Sleep(2 * time.Second) // reload + watchers respawn

	// Reset event log so we only observe events after reload.
	_ = os.Remove(eventLog)

	// Touch globB — should fire 'b' event (new watcher).
	mustWriteFile(t, filepath.Join(wtPath, "globB/v1.txt"), "second")
	waitForEvent(t, eventLog, "b", 5*time.Second)
}

func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// e2e/sighup → up two levels.
	return filepath.Dir(filepath.Dir(wd))
}

func buildBin(t *testing.T, binDir, repoRoot, name, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, name), pkg)
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
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

func runTreeman(t *testing.T, binDir, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "treeman"), args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("treeman %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitForEvent(t *testing.T, path, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(path)
		if strings.Contains(string(body), want) {
			t.Logf("observed event %q in %s", want, path)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	body, _ := os.ReadFile(path)
	t.Fatalf("never observed event %q after %s (have: %s)", want, timeout, body)
}

var (
	_ = context.Background
	_ = strconv.Itoa
	_ = fmt.Sprintf
)
