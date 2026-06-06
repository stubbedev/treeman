//go:build e2e

// Package onfilechange_e2e drives the full daemon-managed watcher
// against a real engine + file-change hook. Each file edit must:
//
//  1. Fire the matching file-change action with env vars
//     describing the event (TREEMAN_WATCH_PATH, _LABEL, _ENGINE,
//     _DB_NAME).
//  2. Trigger FinalizeWorktreeForWatch → new fingerprint in store.
package onfilechange_e2e

import (
	"context"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

func TestOnFileChangeHookFires(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "mysql:13336", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13336", 1*time.Second)
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
	if err := os.MkdirAll(filepath.Join(repoRoot, "db/migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "db/migrations/000_init.sql"),
		[]byte("CREATE TABLE x(id INT);"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".env"),
		[]byte("MYSQL_PW=rootpw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yaml := `
worktrees:
  root: .worktrees
env_sources: [.env]
debounce_ms: 200
connections:
  mysql:
    host: 127.0.0.1
    port: 13336
    user: root
    password: $MYSQL_PW
databases:
  - engine: mysql
    name_template: tm_ofc_{slug}
    inputs:
      - { glob: "db/migrations/*.sql", label: migrations }
hooks:
  file-change:
    - match: migrations
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
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := daemon.NewState(ctx, st)
	rawDB, _ := sql.Open("sqlite", dbPath)
	defer rawDB.Close()

	env := map[string]string{
		"PATH":     os.Getenv("PATH"),
		"MYSQL_PW": "rootpw",
	}
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env); err != nil {
		t.Fatalf("initial finalize: %v", err)
	}

	// Spin up the per-worktree watchers (HEAD + FS).
	if err := daemon.ResumeWorktreeWatcher(ctx, state, repoRoot, repoRoot); err != nil {
		t.Fatalf("ResumeWorktreeWatcher: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // let fsnotify subscribe

	// Add a new migration → expect:
	//   • file-change hook fires (writes touch/event)
	//   • FinalizeWorktreeForWatch updates fingerprint
	if err := os.WriteFile(filepath.Join(repoRoot, "db/migrations/001_new.sql"),
		[]byte("ALTER TABLE x ADD COLUMN n INT;"), 0o644); err != nil {
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
		t.Fatal("file-change hook never wrote touch/event")
	}
	t.Logf("event body: %s", strings.TrimSpace(body))
	for _, want := range []string{"db/migrations/001_new.sql", "migrations", "mysql", "tm_ofc_"} {
		if !strings.Contains(body, want) {
			t.Errorf("event body missing %q: %s", want, body)
		}
	}
}
