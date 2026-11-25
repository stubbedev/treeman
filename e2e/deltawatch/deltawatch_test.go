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
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

type fixture struct {
	repoRoot string
	state    *daemon.State
	rawDB    *sql.DB
	env      map[string]string
}

// newFixture brings up the mysql stack and a repo whose single mysql
// database watches db/migrations/*.sql. extraYAML is appended to the
// generated .treeman.yaml.
func newFixture(t *testing.T, extraYAML string) fixture {
	t.Helper()
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "mysql:13327", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13327", 1*time.Second)
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
	yaml := `
worktrees:
  root: .worktrees
env_sources: [.env]
connections:
  mysql:
    host: 127.0.0.1
    port: 13327
    user: root
    password: $MYSQL_PW
databases:
  - engine: mysql
    name_template: tm_dw_{slug}
    inputs:
      - { glob: "db/migrations/*.sql", label: migrations }
` + extraYAML
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
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })

	return fixture{
		repoRoot: repoRoot,
		state:    daemon.NewState(ctx, st, config.DefaultLifecycleSlots),
		rawDB:    rawDB,
		env: map[string]string{
			"PATH":     os.Getenv("PATH"),
			"MYSQL_PW": "rootpw",
		},
	}
}

func TestDeltaWatcherRePrepsAfterInputEdit(t *testing.T) {
	f := newFixture(t, "")
	ctx := context.Background()

	// Initial finalize → fingerprint F1
	if err := daemon.FinalizeWorktree(ctx, f.state, f.repoRoot, f.repoRoot, f.env, false); err != nil {
		t.Fatalf("initial finalize: %v", err)
	}
	fp1 := mostRecentFingerprint(t, f.rawDB)
	if fp1 == "" {
		t.Fatal("no snapshot recorded after initial finalize")
	}
	t.Logf("F1 = %s", fp1)

	// Add a new migration → input fingerprint should change
	if err := os.WriteFile(filepath.Join(f.repoRoot, "db/migrations/002_add_col.sql"),
		[]byte("ALTER TABLE x ADD COLUMN n INT;"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate the watcher dispatch (daemon's real path is the same
	// FinalizeWorktreeForWatch call from makeWtFSDispatcher).
	if err := daemon.FinalizeWorktreeForWatch(ctx, f.state, f.repoRoot, f.repoRoot, 0, f.env); err != nil {
		t.Fatalf("watch-triggered finalize: %v", err)
	}
	fp2 := mostRecentFingerprint(t, f.rawDB)
	if fp2 == "" {
		t.Fatal("no snapshot recorded after watch-triggered finalize")
	}
	t.Logf("F2 = %s", fp2)

	if fp1 == fp2 {
		t.Errorf("fingerprint unchanged after migration add: %s == %s", fp1, fp2)
	}
}

// TestDeltaWatcherReplaysEditDuringInFlightFinalize pins issue #128:
// an input edit whose watch event lands while another finalize of the
// worktree is still running — after that run fingerprinted the inputs
// — must still be built once that run finishes, not dropped.
func TestDeltaWatcherReplaysEditDuringInFlightFinalize(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release")
	f := newFixture(t, fmt.Sprintf(`hooks:
  create-after-engines:
    - run: "while [ ! -e %s ]; do sleep 0.1; done"
`, gate))
	ctx := context.Background()

	finalized := make(chan error, 1)
	go func() {
		finalized <- daemon.FinalizeWorktree(ctx, f.state, f.repoRoot, f.repoRoot, f.env, false)
	}()
	var fp1 string
	harness.WaitForReady(t, "initial snapshot", 2*time.Minute, func() error {
		if fp1 = mostRecentFingerprint(t, f.rawDB); fp1 == "" {
			return errors.New("no snapshot yet")
		}
		return nil
	})

	if err := os.WriteFile(filepath.Join(f.repoRoot, "db/migrations/002_add_col.sql"),
		[]byte("ALTER TABLE x ADD COLUMN n INT;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := daemon.FinalizeWorktreeForWatch(ctx, f.state, f.repoRoot, f.repoRoot, 0, f.env); err != nil {
		t.Fatalf("watch-triggered finalize during in-flight finalize: %v", err)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-finalized; err != nil {
		t.Fatalf("initial finalize: %v", err)
	}

	harness.WaitForReady(t, "snapshot of the edited inputs", 2*time.Minute, func() error {
		if fp := mostRecentFingerprint(t, f.rawDB); fp == fp1 {
			return fmt.Errorf("latest snapshot still %s", fp1)
		}
		return nil
	})
	if err := f.state.WaitFinalizeCleared(ctx, f.repoRoot, 2*time.Minute); err != nil {
		t.Fatal(err)
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
