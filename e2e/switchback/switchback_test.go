//go:build e2e

// Package switchback_e2e verifies the cache survives a worktree
// branch switch round-trip: A → B → A. After visiting B, returning
// to A's input state must be a CACHE HIT — the snapshot row for A's
// fingerprint is still in SQLite and the template DB still exists
// in MySQL.
//
// This is the canonical fast-switch user experience: bouncing
// between feature branches with non-trivial schema diffs.
package switchback_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

func TestSwitchBackIsCacheHit(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13376", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13376", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wtA := writeWT(t, []byte(`
CREATE TABLE alpha (id INT PRIMARY KEY, val VARCHAR(32));
INSERT INTO alpha VALUES (1, 'one'), (2, 'two');
`))
	wtB := writeWT(t, []byte(`
CREATE TABLE beta (id INT PRIMARY KEY, val VARCHAR(32));
INSERT INTO beta VALUES (10, 'ten'), (20, 'twenty');
`))

	mkCfg := func() *config.Config {
		return &config.Config{
			Connections: config.ConnectionsConfig{
				Mysql: &config.MysqlConn{
					Host: "127.0.0.1", Port: 13376,
					User: "root", Password: "rootpw",
				},
			},
			Databases: []config.DatabaseConfig{
				{
					Engine:       "mysql",
					NameTemplate: "tm_swb_{slug}",
					Dump:         &config.DumpSpec{Path: "seed.sql"},
				},
			},
		}
	}

	// Shared store so cache state survives across worktree switches.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	envA := mkEnv(t, st, wtA, "feature-a")
	envB := mkEnv(t, st, wtB, "feature-b")

	// ── 1. Cold build at A ──
	outsA1 := envA.RunPrepare(t, mkCfg())
	a1 := harness.AssertOutcome(t, outsA1, "mysql", false)
	t.Logf("A1 cold: source=%s fp=%s", a1.SourceDB, a1.Fingerprint[:12])
	assertCount(t, a1.SourceDB, "alpha", 2)

	// ── 2. Switch to B → cold build ──
	outsB1 := envB.RunPrepare(t, mkCfg())
	b1 := harness.AssertOutcome(t, outsB1, "mysql", false)
	if b1.Fingerprint == a1.Fingerprint {
		t.Fatal("A and B fingerprints coincide — test setup wrong")
	}
	t.Logf("B1 cold: source=%s fp=%s", b1.SourceDB, b1.Fingerprint[:12])
	assertCount(t, b1.SourceDB, "beta", 2)
	// A's snapshot row must still exist after B's run.
	rec, _ := st.LookupSnapshot(ctx, a1.Fingerprint)
	if rec == nil {
		t.Fatal("A's snapshot row was evicted by B's prepare")
	}

	// ── 3. Switch BACK to A → cache hit ──
	outsA2 := envA.RunPrepare(t, mkCfg())
	a2 := harness.AssertOutcome(t, outsA2, "mysql", true)
	if a2.Fingerprint != a1.Fingerprint {
		t.Errorf("revisiting A: fingerprint drift %s → %s",
			a1.Fingerprint[:12], a2.Fingerprint[:12])
	}
	t.Logf("A2 hit:  fp=%s (same as A1)", a2.Fingerprint[:12])
	assertCount(t, a2.SourceDB, "alpha", 2)

	// ── 4. Switch back to B → cache hit ──
	outsB2 := envB.RunPrepare(t, mkCfg())
	b2 := harness.AssertOutcome(t, outsB2, "mysql", true)
	if b2.Fingerprint != b1.Fingerprint {
		t.Errorf("revisiting B: fingerprint drift")
	}
	t.Logf("B2 hit:  fp=%s (same as B1)", b2.Fingerprint[:12])
	assertCount(t, b2.SourceDB, "beta", 2)
}

func mkEnv(t *testing.T, st *store.Store, wt, branch string) *harness.Env {
	t.Helper()
	ctx := context.Background()
	repoID, err := st.EnsureRepo(ctx, wt, filepath.Base(wt))
	if err != nil {
		t.Fatal(err)
	}
	sl := slug.For(wt, branch)
	wtID, err := st.EnsureWorktree(ctx, repoID, wt, sl.Value, branch)
	if err != nil {
		t.Fatal(err)
	}
	return &harness.Env{
		Ctx: ctx, Store: st, RepoID: repoID, WTID: wtID,
		Slug: sl, RepoPath: wt, WTPath: wt,
	}
}

func writeWT(t *testing.T, seed []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.sql"), seed, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertCount(t *testing.T, dbName, table string, want int) {
	t.Helper()
	dsn := fmt.Sprintf("root:rootpw@tcp(127.0.0.1:13376)/%s", dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s.%s: %v", dbName, table, err)
	}
	if n != want {
		t.Errorf("%s.%s rows = %d, want %d", dbName, table, n, want)
	}
}
