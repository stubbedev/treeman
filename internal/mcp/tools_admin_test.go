package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDaemonControl_InvalidAction — the action validator rejects unknown
// actions before any RPC/daemonctl call.
func TestDaemonControl_InvalidAction(t *testing.T) {
	if _, _, err := daemonControlTool(context.Background(), nil, daemonControlIn{Action: "frobnicate"}); err == nil {
		t.Fatalf("expected error for unknown action")
	}
}

// TestMainWorktree_InvalidAction — likewise for main_worktree.
func TestMainWorktree_InvalidAction(t *testing.T) {
	repo := newTempRepo(t)
	if _, _, err := mainWorktreeTool(context.Background(), nil, mainWorktreeIn{Action: "nope", Repo: repo}); err == nil {
		t.Fatalf("expected error for unknown action")
	}
}

// TestMainWorktree_PatchEnables — enable flips main_worktree.enabled in
// .treeman.yaml even with no daemon running (reload failure is tolerated
// and surfaced in Detail, not returned as an error).
func TestMainWorktree_PatchEnables(t *testing.T) {
	repo := newTempRepo(t)
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "t.db"))

	if err := patchMainWorktreeEnabled(repo, true); err != nil {
		t.Fatalf("patchMainWorktreeEnabled: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".treeman.yaml"))
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(body), "enabled: true") {
		t.Errorf("main_worktree.enabled not set:\n%s", body)
	}
	// Flip back off.
	if err := patchMainWorktreeEnabled(repo, false); err != nil {
		t.Fatalf("patch off: %v", err)
	}
	body, _ = os.ReadFile(filepath.Join(repo, ".treeman.yaml"))
	if !strings.Contains(string(body), "enabled: false") {
		t.Errorf("main_worktree.enabled not cleared:\n%s", body)
	}
}

// TestWorktreeFinalize_UnknownIdentifier — a stale slug that matches no
// registered worktree must error like logs_query does, not resolve to
// <cwd>/<name> and report queued=true (issue #23).
func TestWorktreeFinalize_UnknownIdentifier(t *testing.T) {
	repo := newTempRepo(t)
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "t.db"))

	_, out, err := worktreeFinalizeTool(context.Background(), nil, worktreeFinalizeIn{
		Worktree: "no_such_worktree_slug",
		Repo:     repo,
	})
	if err == nil {
		t.Fatalf("expected error, got queued=%v path=%q", out.Queued, out.WorktreePath)
	}
	if !strings.Contains(err.Error(), "no worktree matches") {
		t.Errorf("error should mention 'no worktree matches', got: %v", err)
	}
}

// TestNotifyTest_NoneRejected — backend "none" mutes everything, so the
// test refuses it (before touching any sender).
func TestNotifyTest_NoneRejected(t *testing.T) {
	if _, _, err := notifyTestTool(context.Background(), nil, notifyTestIn{Backend: "none"}); err == nil {
		t.Fatalf("expected error for backend=none")
	}
}
