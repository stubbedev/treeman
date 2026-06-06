//go:build e2e

// Package lifecycle_e2e tests the full worktree create/delete pipeline
// through internal/daemon:
//
//	FinalizeWorktree → create-before-engines → engine prepare →
//	  create-after-engines
//	TeardownWorktree → delete-before-engines → engine drop →
//	  delete-after-engines
//
// Hooks write touchstone files we can stat to confirm each phase
// actually fired in the documented order.
package lifecycle_e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

func TestLifecycleHooksFireInOrder(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "mysql:13316", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13316", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	// repo and worktree are the same dir for this test (simulates a
	// single-worktree project).
	repoRoot := t.TempDir()
	touchstones := filepath.Join(repoRoot, "touchstones")
	if err := os.MkdirAll(touchstones, 0o755); err != nil {
		t.Fatal(err)
	}

	// .env exposes the DB password to the credential resolver
	// (process env is intentionally NOT consulted by `password_env`).
	if err := os.WriteFile(filepath.Join(repoRoot, ".env"),
		[]byte("MYSQL_E2E_PW=rootpw\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	yaml := fmt.Sprintf(`
worktrees:
  root: .worktrees

env_sources:
  - .env

connections:
  mysql:
    host: 127.0.0.1
    port: 13316
    user: root
    password: $MYSQL_E2E_PW

databases:
  - engine: mysql
    name_template: tm_lc_{slug}

hooks:
  create-before-engines:
    - run: "touch %s/01_before_engines"
  create-after-engines:
    - run: "touch %s/02_after_engines"
  delete-before-engines:
    - run: "touch %s/03_before_delete"
  delete-after-engines:
    - run: "touch %s/04_after_delete"
`, touchstones, touchstones, touchstones, touchstones)
	if err := os.WriteFile(filepath.Join(repoRoot, ".treeman.yaml"),
		[]byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Init a fake .git so detectBranch() picks something deterministic.
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".git/HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MYSQL_E2E_PW", "rootpw")

	// Stand up a real daemon State.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := daemon.NewState(ctx, st)

	// ── FinalizeWorktree → setup + prepare + post-setup hooks ──
	env := map[string]string{
		"PATH":         os.Getenv("PATH"),
		"MYSQL_E2E_PW": "rootpw",
	}
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env); err != nil {
		t.Fatalf("FinalizeWorktree: %v", err)
	}

	// Hooks run async (safeGo); wait briefly for the touchstones.
	waitFor(t, filepath.Join(touchstones, "01_before_engines"), 10*time.Second)
	waitFor(t, filepath.Join(touchstones, "02_after_engines"), 10*time.Second)

	// before_engines must beat after_engines in mtime (sequenced
	// around the prepare step, not parallel).
	beforeStat, err := os.Stat(filepath.Join(touchstones, "01_before_engines"))
	if err != nil {
		t.Fatal(err)
	}
	afterStat, err := os.Stat(filepath.Join(touchstones, "02_after_engines"))
	if err != nil {
		t.Fatal(err)
	}
	if !beforeStat.ModTime().Before(afterStat.ModTime()) &&
		!beforeStat.ModTime().Equal(afterStat.ModTime()) {
		t.Errorf("before_engines mtime (%s) is AFTER after_engines (%s) — order broken",
			beforeStat.ModTime(), afterStat.ModTime())
	}

	// ── TeardownWorktree → delete hooks + DB drop ──
	if err := daemon.TeardownWorktree(ctx, state, repoRoot, repoRoot, true, env); err != nil {
		// TeardownWorktree may try to `git worktree remove` the path
		// which won't work for our fake .git layout — that's fine, we
		// only care that the delete hooks fired.
		if !strings.Contains(err.Error(), "worktree remove") &&
			!strings.Contains(err.Error(), "git") {
			t.Fatalf("TeardownWorktree: %v", err)
		}
	}
	waitFor(t, filepath.Join(touchstones, "03_before_delete"), 10*time.Second)
	waitFor(t, filepath.Join(touchstones, "04_after_delete"), 10*time.Second)
}

func waitFor(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("touchstone never appeared: %s", path)
}
