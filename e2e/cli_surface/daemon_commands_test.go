//go:build e2e

// Exercises the CLI surface of the daemon/engine-adjacent commands that
// don't need a running daemon or engine to test their plumbing:
//   - `main enable|disable|status` (config write + store read; graceful
//     when no daemon is up)
//   - `sync` / `sync --status` (clean error when no daemon is reachable)
//   - `prepare` with no databases (loads config, runs the no-op prepare)
//
// The full engine-backed paths live in e2e/cli (real daemon + MySQL).
package cli_surface_e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainEnableDisableStatus(t *testing.T) {
	repo := newGitRepo(t)
	e := newEnv(t)

	// enable: writes main_worktree.enabled: true; the daemon reload RPC
	// fails (no daemon) but the command degrades gracefully (exit 0).
	res := e.run(t, repo, "main", "enable")
	if res.err != nil {
		t.Fatalf("main enable: %v\nstderr:\n%s", res.err, res.stderr)
	}
	yaml, _ := os.ReadFile(filepath.Join(repo, ".treeman.yaml"))
	if !strings.Contains(string(yaml), "main_worktree") || !strings.Contains(string(yaml), "true") {
		t.Errorf("main enable did not write enabled:true:\n%s", yaml)
	}

	// status: reads config + store; no enrollment row yet.
	res = e.run(t, repo, "main", "status")
	if res.err != nil {
		t.Fatalf("main status: %v\nstderr:\n%s", res.err, res.stderr)
	}
	if !strings.Contains(res.stdout, "enabled") {
		t.Errorf("main status missing enabled line:\n%s", res.stdout)
	}

	// status --json shape.
	res = e.run(t, repo, "main", "status", "--json")
	if res.err != nil {
		t.Fatalf("main status --json: %v\nstderr:\n%s", res.err, res.stderr)
	}
	if !strings.Contains(res.stdout, "{") {
		t.Errorf("main status --json not JSON:\n%s", res.stdout)
	}

	// disable: flips enabled back to false.
	res = e.run(t, repo, "main", "disable")
	if res.err != nil {
		t.Fatalf("main disable: %v\nstderr:\n%s", res.err, res.stderr)
	}
	yaml, _ = os.ReadFile(filepath.Join(repo, ".treeman.yaml"))
	if !strings.Contains(string(yaml), "false") {
		t.Errorf("main disable did not write enabled:false:\n%s", yaml)
	}
}

func TestSyncNoDaemonErrorsCleanly(t *testing.T) {
	repo := newGitRepo(t)
	e := newEnv(t)

	// No daemon running → sync_now RPC can't connect → clean error exit.
	res := e.run(t, repo, "sync")
	if res.err == nil {
		t.Errorf("sync with no daemon should exit non-zero, got success:\n%s", res.stdout)
	}

	// --status path also routes through the daemon.
	res = e.run(t, repo, "sync", "--status")
	if res.err == nil {
		t.Errorf("sync --status with no daemon should exit non-zero, got success:\n%s", res.stdout)
	}
}

func TestPrepareNoDatabases(t *testing.T) {
	repo := newGitRepo(t)
	e := newEnv(t)
	// A config with no databases: prepare loads it, registers the repo,
	// and runs the (empty) prepare without an engine — exit 0.
	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"),
		[]byte("worktrees:\n  root: .worktrees\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, repo, "prepare")
	if res.err != nil {
		t.Fatalf("prepare (no databases) should be a clean no-op: %v\nstderr:\n%s", res.err, res.stderr)
	}
}
