//go:build e2e

// Package deltawatch_e2e proves that an input-file edit propagates
// end-to-end through the daemon's watcher to a fresh engine
// snapshot. Sequence:
//
//  1. Initial FinalizeWorktree → fingerprint F1, snapshot row F1
//  2. Edit migration file under a watched glob
//  3. FinalizeWorktreeForWatch fires (simulated via direct call,
//     matching the watcher.Dispatcher callback signature)
//  4. New fingerprint F2 ≠ F1; new snapshot row exists; old still
//     present until cap eviction
package deltawatch_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

func TestDeltaWatcherRePrepsAfterInputEdit(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "mysql:13326", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13326", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "db/migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "db/migrations/001_init.sql"),
		[]byte("CREATE TABLE x (id INT);"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".env"),
		[]byte("MYSQL_PW=rootpw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf(`
worktrees:
  root: .worktrees
env_sources: [.env]
connections:
  mysql:
    host: 127.0.0.1
    port: 13326
    user: root
    password: $MYSQL_PW
databases:
  - engine: mysql
    name_template: tm_dw_{slug}
    inputs:
      - { glob: "db/migrations/*.sql", label: migrations, hash: filename }
`)
	if err := os.WriteFile(filepath.Join(repoRoot, ".treeman.yaml"),
		[]byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	// minimal fake .git
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".git/HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	storeDir := t.TempDir()
	dbPath := filepath.Join(storeDir, "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := daemon.NewState(ctx, st)

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()

	env := map[string]string{
		"PATH":     os.Getenv("PATH"),
		"MYSQL_PW": "rootpw",
	}

	// Initial finalize → fingerprint F1
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env); err != nil {
		t.Fatalf("initial finalize: %v", err)
	}
	fp1 := mostRecentFingerprint(t, rawDB)
	if fp1 == "" {
		t.Fatal("no snapshot recorded after initial finalize")
	}
	t.Logf("F1 = %s", fp1)

	// Add a new migration → input fingerprint should change
	if err := os.WriteFile(filepath.Join(repoRoot, "db/migrations/002_add_col.sql"),
		[]byte("ALTER TABLE x ADD COLUMN n INT;"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate the watcher dispatch (daemon's real path is the same
	// FinalizeWorktreeForWatch call from makeWtFSDispatcher).
	if err := daemon.FinalizeWorktreeForWatch(ctx, state, repoRoot, repoRoot, 0, env); err != nil {
		t.Fatalf("watch-triggered finalize: %v", err)
	}
	fp2 := mostRecentFingerprint(t, rawDB)
	if fp2 == "" {
		t.Fatal("no snapshot recorded after watch-triggered finalize")
	}
	t.Logf("F2 = %s", fp2)

	if fp1 == fp2 {
		t.Errorf("fingerprint unchanged after migration add: %s == %s", fp1, fp2)
	}
}

// mostRecentFingerprint queries the snapshots table directly. We
// avoid leaning on the store API since LookupSnapshot is keyed by
// fingerprint (which we don't know up front in this test).
func mostRecentFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	row := db.QueryRow(`SELECT fingerprint FROM snapshots ORDER BY created_at DESC LIMIT 1`)
	var fp string
	if err := row.Scan(&fp); err != nil {
		if err == sql.ErrNoRows {
			return ""
		}
		t.Fatalf("query snapshot: %v", err)
	}
	return fp
}
