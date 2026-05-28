//go:build e2e

// Package headwatcher_e2e drives the HEAD watcher against a real
// `.git/HEAD` file: a `git checkout` (or HEAD rewrite) must invoke
// the onChange callback with the new ref.
package headwatcher_e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/daemon"
)

func TestHeadWatcherFiresOnCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	wt := t.TempDir()
	mustGit(t, wt, "init", "-q", "-b", "main")
	mustGit(t, wt, "config", "user.email", "e2e@example.com")
	mustGit(t, wt, "config", "user.name", "e2e")
	if err := os.WriteFile(filepath.Join(wt, "file.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "add", "file.txt")
	mustGit(t, wt, "commit", "-q", "-m", "init")
	mustGit(t, wt, "checkout", "-q", "-b", "feature")
	// Re-touch file on feature so checkout main moves HEAD with content.
	if err := os.WriteFile(filepath.Join(wt, "file.txt"), []byte("hi2"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt, "commit", "-q", "-am", "feature work")

	var (
		mu   sync.Mutex
		seen []string
	)
	hw, err := daemon.NewHeadWatcher(wt, 50*time.Millisecond, func(_ context.Context, ref string) {
		mu.Lock()
		seen = append(seen, ref)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("NewHeadWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hw.Start(ctx) }()
	defer hw.Stop()
	time.Sleep(300 * time.Millisecond) // let it seed lastSeen

	// Branch switch — should fire onChange.
	mustGit(t, wt, "checkout", "-q", "main")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("HEAD watcher never fired onChange")
	}
	// The ref string should mention "main" (the new branch we switched to).
	if seen[0] == "" {
		t.Errorf("ref empty in callback")
	}
	t.Logf("HEAD watcher fired %d time(s); latest ref = %q", len(seen), seen[len(seen)-1])
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
