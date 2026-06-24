package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func requireGitHW(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// TestHeadWatcherFiresOnBranchSwitch end-to-ends a real git repo:
// init, commit, watch HEAD, checkout -b, assert onChange ran.
func TestHeadWatcherFiresOnBranchSwitch(t *testing.T) {
	requireGitHW(t)
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-m", "init")

	var fired atomic.Int32
	got := make(chan string, 1)
	hw, err := NewHeadWatcher(repo, 50*time.Millisecond, func(_ context.Context, ref string) {
		fired.Add(1)
		select {
		case got <- ref:
		default:
		}
	})
	if err != nil {
		t.Fatalf("NewHeadWatcher: %v", err)
	}
	ctx := t.Context()
	defer hw.Stop()
	go func() { _ = hw.Start(ctx) }()
	// Give Start a beat to subscribe + seed lastSeen.
	time.Sleep(100 * time.Millisecond)

	run("checkout", "-q", "-b", "feature")

	select {
	case ref := <-got:
		if !contains(ref, "refs/heads/feature") {
			t.Errorf("ref = %q, want feature", ref)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HEAD change did not fire within 2s")
	}
}

// TestHeadWatcherDebouncesRapidChanges checks that multiple rapid
// writes within the debounce window coalesce into a single dispatch.
func TestHeadWatcherDebouncesRapidChanges(t *testing.T) {
	requireGitHW(t)
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-m", "init")
	run("checkout", "-q", "-b", "a")

	var fired atomic.Int32
	hw, err := NewHeadWatcher(repo, 200*time.Millisecond, func(_ context.Context, _ string) {
		fired.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	defer hw.Stop()
	go func() { _ = hw.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)

	// Rapid: 3 checkouts in quick succession; debounce should
	// collapse the dispatch count to roughly 1 (the timer resets
	// on every event).
	run("checkout", "-q", "-b", "b")
	run("checkout", "-q", "a")
	run("checkout", "-q", "b")

	// Wait well past the debounce window to let the dispatch fire.
	time.Sleep(500 * time.Millisecond)

	if n := fired.Load(); n != 1 {
		t.Errorf("fired = %d, want 1 (debounced)", n)
	}
}

// TestHeadWatcherDefersAndRecoversOnGitOp simulates a conflicted merge:
// while MERGE_HEAD exists the watcher must NOT dispatch (finalize would
// only fail against the conflicted tree), and when MERGE_HEAD clears —
// without HEAD ever moving, as in `git merge --abort` — it must fire
// exactly once so the worktree re-prepares and recovers.
func TestHeadWatcherDefersAndRecoversOnGitOp(t *testing.T) {
	requireGitHW(t)
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-m", "init")
	gitDir := filepath.Join(repo, ".git")
	mergeHead := filepath.Join(gitDir, "MERGE_HEAD")

	var fired atomic.Int32
	hw, err := NewHeadWatcher(repo, 50*time.Millisecond, func(_ context.Context, _ string) {
		fired.Add(1)
	})
	if err != nil {
		t.Fatalf("NewHeadWatcher: %v", err)
	}
	ctx := t.Context()
	defer hw.Stop()
	go func() { _ = hw.Start(ctx) }()
	// Let Start subscribe + seed the clean baseline (lastSeen + lastOp).
	time.Sleep(100 * time.Millisecond)

	// Merge in progress — HEAD unchanged, MERGE_HEAD appears.
	if err := os.WriteFile(mergeHead, []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if n := fired.Load(); n != 0 {
		t.Fatalf("fired = %d while merge in progress, want 0 (deferred)", n)
	}

	// Abort/resolve — MERGE_HEAD removed, HEAD still unchanged.
	if err := os.Remove(mergeHead); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if n := fired.Load(); n != 1 {
		t.Errorf("fired = %d after merge cleared, want 1 (recovery re-run)", n)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestResolveHeadPathHandlesLinkedWorktree confirms the gitlink path
// resolves correctly for `git worktree add` checkouts.
func TestResolveHeadPathHandlesLinkedWorktree(t *testing.T) {
	requireGitHW(t)
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	run(repo, "commit", "--allow-empty", "-m", "init")
	wt := filepath.Join(tmp, "wt")
	run(repo, "worktree", "add", wt, "-b", "feature")

	got, err := resolveHeadPath(wt)
	if err != nil {
		t.Fatalf("resolveHeadPath: %v", err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("resolved HEAD path missing: %s (%v)", got, err)
	}
	body, _ := os.ReadFile(got)
	if !contains(string(body), "refs/heads/feature") {
		t.Errorf("resolved HEAD does not point at feature: %q", body)
	}
}
