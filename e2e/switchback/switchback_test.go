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
					Dump:         config.DumpList{{Path: "seed.sql"}},
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

// TestCrossWorktreeCacheReuse pins the v5 win: two DIFFERENT worktrees
// (different slugs → different source DB names) with IDENTICAL inputs
// share ONE template. The first cold-builds; the second is a CACHE HIT
// off the first's template — no redundant rebuild. It also covers the
// folded source restore: the second worktree's own bare source DB is
// populated on the cache hit (the source is restored as part of the
// clone fan-out, not skipped).
//
// Before v5 the source DB name was mixed into the fingerprint, so these
// two worktrees produced distinct fingerprints and BOTH cold-built —
// this test would have failed at the second AssertOutcome.
func TestCrossWorktreeCacheReuse(t *testing.T) {
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

	seed := []byte(`
CREATE TABLE gamma (id INT PRIMARY KEY, val VARCHAR(32));
INSERT INTO gamma VALUES (1, 'one'), (2, 'two'), (3, 'three');
`)
	// Identical content, two distinct worktree checkouts.
	wt1 := writeWT(t, seed)
	wt2 := writeWT(t, seed)

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
					NameTemplate: "tm_xwt_{slug}",
					Dump:         config.DumpList{{Path: "seed.sql"}},
				},
			},
		}
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	env1 := mkEnv(t, st, wt1, "feature-one")
	env2 := mkEnv(t, st, wt2, "feature-two")

	// First worktree: cold build, creates the shared template.
	o1 := harness.AssertOutcome(t, env1.RunPrepare(t, mkCfg()), "mysql", false)
	t.Logf("wt1 cold: source=%s fp=%s tmpl=%s", o1.SourceDB, o1.Fingerprint[:12], o1.TemplateName)
	assertCount(t, o1.SourceDB, "gamma", 3)

	// Second worktree, identical content but a different slug: CACHE HIT.
	o2 := harness.AssertOutcome(t, env2.RunPrepare(t, mkCfg()), "mysql", true)
	t.Logf("wt2 hit:  source=%s fp=%s tmpl=%s", o2.SourceDB, o2.Fingerprint[:12], o2.TemplateName)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("identical inputs must share a fingerprint: %s vs %s",
			o1.Fingerprint[:12], o2.Fingerprint[:12])
	}
	if o2.TemplateName != o1.TemplateName {
		t.Errorf("identical inputs must share a template: %s vs %s",
			o1.TemplateName, o2.TemplateName)
	}
	if o2.SourceDB == o1.SourceDB {
		t.Errorf("distinct worktrees must keep distinct source DBs: both %s", o2.SourceDB)
	}
	// Folded source restore: wt2's own bare source DB is populated.
	assertCount(t, o2.SourceDB, "gamma", 3)
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
