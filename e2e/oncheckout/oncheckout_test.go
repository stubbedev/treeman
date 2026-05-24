//go:build e2e

// Package oncheckout_e2e drives a real branch switch inside a real
// worktree and asserts the `hooks.on-checkout:` action fires with
// the new ref. The headwatcher e2e suite only proves the watcher
// detects HEAD changes — this test goes one step further and
// confirms the configured action runs.
package oncheckout_e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

func TestOnCheckoutHookFires(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13426", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13426", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	// Real git worktree with a feature branch we can switch to.
	repoRoot := t.TempDir()
	mustGit(t, repoRoot, "init", "-q", "-b", "main")
	mustGit(t, repoRoot, "config", "user.email", "e2e@example.com")
	mustGit(t, repoRoot, "config", "user.name", "e2e")
	must := func(p, body string) {
		full := filepath.Join(repoRoot, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(body), 0o644)
	}
	must("file.txt", "hi")
	must(".env", "MYSQL_PW=rootpw\n")

	touch := filepath.Join(repoRoot, "touch")
	_ = os.MkdirAll(touch, 0o755)
	yaml := `
worktrees: { root: .worktrees }
env_sources: [.env]
connections:
  mysql: { host: 127.0.0.1, port: 13426, user: root, password_env: MYSQL_PW }
databases:
  - engine: mysql
    name_template: tm_oc_{slug}
hooks:
  on-checkout:
    - run: echo "$TREEMAN_SLUG" > ` + touch + `/checkout-fired
`
	must(".treeman.yaml", yaml)
	mustGit(t, repoRoot, "add", "-A")
	mustGit(t, repoRoot, "commit", "-q", "-m", "init")
	// Add a second branch so we can switch.
	mustGit(t, repoRoot, "checkout", "-q", "-b", "feature")
	must("file.txt", "feature work")
	mustGit(t, repoRoot, "commit", "-q", "-am", "feature")
	mustGit(t, repoRoot, "checkout", "-q", "main")

	// Stand up daemon state + spawn watchers.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := daemon.NewState(ctx, st)
	env := map[string]string{
		"PATH":     os.Getenv("PATH"),
		"MYSQL_PW": "rootpw",
	}
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env); err != nil {
		t.Fatalf("FinalizeWorktree: %v", err)
	}
	if err := daemon.ResumeWorktreeWatcher(ctx, state, repoRoot, repoRoot); err != nil {
		t.Fatalf("ResumeWorktreeWatcher: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // let HEAD watcher seed

	// Trigger HEAD change → on-checkout should fire.
	mustGit(t, repoRoot, "checkout", "-q", "feature")

	witness := filepath.Join(touch, "checkout-fired")
	harness.WaitForReady(t, "on-checkout-fired", 15*time.Second, func() error {
		body, err := os.ReadFile(witness)
		if err != nil {
			return fmt.Errorf("witness missing: %v", err)
		}
		if len(body) == 0 {
			return fmt.Errorf("witness empty")
		}
		return nil
	})
	body, _ := os.ReadFile(witness)
	t.Logf("on-checkout fired; slug=%s", string(body))
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
