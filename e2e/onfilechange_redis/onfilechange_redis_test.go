//go:build e2e

// Package onfilechange_redis_e2e is the prefix-engine twin of the
// (relational) onfilechange suite: it proves the daemon-managed
// `inputs` watcher fires for a NON-SQL engine too, and — critically —
// that file-change hooks for a key_prefix engine receive a
// populated TREEMAN_WATCH_DB_NAME. Prefix engines have no
// name_template, so dispatch falls back to the rendered key_prefix;
// this test is the regression guard for that fallback.
package onfilechange_redis_e2e

import (
	"context"
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

func TestRedisOnFileChangeHookFires(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))

	harness.WaitForReady(t, "redis:16395", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:16395", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	repoRoot := t.TempDir()
	touch := filepath.Join(repoRoot, "touch")
	if err := os.MkdirAll(touch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, "seeds"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "seeds/000_init.txt"),
		[]byte("k1=v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yaml := `
worktrees:
  root: .worktrees
debounce_ms: 200
connections:
  redis:
    url: redis://127.0.0.1:16395
databases:
  - engine: redis
    key_prefix: "tm_ofcr_{slug}:"
    inputs:
      - { glob: "seeds/*.txt", label: seeds }
hooks:
  file-change:
    - match: seeds
      run: 'echo "$TREEMAN_WATCH_PATH|$TREEMAN_WATCH_LABEL|$TREEMAN_WATCH_ENGINE|$TREEMAN_WATCH_DB_NAME" > ` + touch + `/event'
`
	if err := os.WriteFile(filepath.Join(repoRoot, ".treeman.yaml"),
		[]byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".git/HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := daemon.NewState(ctx, st)

	env := map[string]string{"PATH": os.Getenv("PATH")}
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env); err != nil {
		t.Fatalf("initial finalize: %v", err)
	}
	if err := daemon.ResumeWorktreeWatcher(ctx, state, repoRoot, repoRoot); err != nil {
		t.Fatalf("ResumeWorktreeWatcher: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // let fsnotify subscribe

	if err := os.WriteFile(filepath.Join(repoRoot, "seeds/001_new.txt"),
		[]byte("k2=v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eventPath := filepath.Join(touch, "event")
	deadline := time.Now().Add(15 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(eventPath)
		if err == nil && len(b) > 0 {
			body = string(b)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("redis file-change hook never wrote touch/event")
	}
	t.Logf("event body: %s", strings.TrimSpace(body))
	// engine=redis proves the watcher fired for a prefix engine;
	// tm_ofcr_ proves TREEMAN_WATCH_DB_NAME fell back to the rendered
	// key_prefix (prefix engines have no name_template).
	for _, want := range []string{"seeds/001_new.txt", "seeds", "redis", "tm_ofcr_"} {
		if !strings.Contains(body, want) {
			t.Errorf("event body missing %q: %s", want, body)
		}
	}
}
