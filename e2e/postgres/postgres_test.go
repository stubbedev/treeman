//go:build e2e

package postgres_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
)

func TestPostgresEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	cfg := buildConfig()
	env := harness.NewEnv(t, wt)

	outs := env.RunPrepare(t, cfg)
	o1 := harness.AssertOutcome(t, outs, "postgres", false)
	t.Logf("pass1: template=%s sourceDB=%s", o1.TemplateName, o1.SourceDB)

	assertTables(t, "127.0.0.1:15432", o1.SourceDB, []string{"products", "orders"})

	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "postgres", true)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("fingerprint drift: %s vs %s", o1.Fingerprint, o2.Fingerprint)
	}

	addMigration(t, wt)
	outs = env.RunPrepare(t, cfg)
	o3 := harness.AssertOutcome(t, outs, "postgres", false)
	if o3.Fingerprint == o1.Fingerprint {
		t.Errorf("fingerprint unchanged after edit")
	}
	assertTables(t, "127.0.0.1:15432", o3.SourceDB, []string{"products", "orders", "shipments"})

	// Fanout setup permutation: the 2 declared clones exist and carry the
	// seeded+migrated schema (restored from the template).
	if len(o3.Clones) != 2 {
		t.Fatalf("fanout: got %d clones, want 2 (%v)", len(o3.Clones), o3.Clones)
	}
	for _, c := range o3.Clones {
		if !pgDBExists(t, "127.0.0.1:15432", c) {
			t.Errorf("clone DB %s missing after fanout", c)
		}
		assertTables(t, "127.0.0.1:15432", c, []string{"products", "orders", "shipments"})
	}

	// ── teardown: `wt delete` drops the per-worktree DBs, keeps the cache ──
	// TeardownDatabases is the DB layer of `treeman wt delete`. It must DROP
	// the source database AND every clone while leaving the fingerprint-keyed
	// template intact, so the next prepare with the same inputs is a cache hit.
	if err := prepare.TeardownDatabases(env.Ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	if pgDBExists(t, "127.0.0.1:15432", o3.SourceDB) {
		t.Errorf("source DB %s still exists after teardown", o3.SourceDB)
	}
	for _, c := range o3.Clones {
		if pgDBExists(t, "127.0.0.1:15432", c) {
			t.Errorf("clone DB %s still exists after teardown", c)
		}
	}
	// The fingerprint-keyed template must SURVIVE teardown so the next
	// worktree with the same inputs still hits the cache.
	if !pgDBExists(t, "127.0.0.1:15432", o3.TemplateName) {
		t.Errorf("template DB %s was dropped by teardown (cache must survive)", o3.TemplateName)
	}
}

// TestMultipleDumpsLoadInOrder covers `dump:` as a SEQUENCE. base.sql
// creates a table; extras.sql inserts rows that reference it. They must
// be loaded in the declared order or the second statement would fail
// against a missing table — proving the loader walks the list serially
// rather than racing or reordering.
func TestMultipleDumpsLoadInOrder(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	fixtures := filepath.Join(wt, "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(fixtures, "base.sql"),
		"CREATE TABLE widgets (id INT PRIMARY KEY, name TEXT);\nINSERT INTO widgets VALUES (1, 'Alpha');\n")
	mustWrite(t, filepath.Join(fixtures, "extras.sql"),
		"INSERT INTO widgets VALUES (2, 'Beta');\nINSERT INTO widgets VALUES (3, 'Gamma');\n")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host: "127.0.0.1", Port: 15432, User: "postgres", Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "postgres",
			NameTemplate: "tm_mdump_{slug}",
			Dump: config.DumpList{
				{Path: "fixtures/base.sql"},
				{Path: "fixtures/extras.sql"},
			},
		}},
	}
	env := harness.NewEnv(t, wt)
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "postgres", false)
	t.Logf("multi-dump cold: source=%s fp=%s", o.SourceDB, o.Fingerprint[:12])
	assertTables(t, "127.0.0.1:15432", o.SourceDB, []string{"widgets"})
	assertWidgetCount(t, "127.0.0.1:15432", o.SourceDB, 3)

	// Reordering the same files MUST flip the fingerprint — the
	// combined hash is order-sensitive by design (different order
	// would mean different DB content). Use InspectFingerprint with a
	// blank engineVersion on both sides so the only varying input is
	// the dump order.
	rep1 := prepare.InspectFingerprint(env.Ctx, env.Store, cfg.Databases[0], wt, o.SourceDB, "")
	swappedCfg := cfg.Databases[0]
	swappedCfg.Dump = config.DumpList{
		{Path: "fixtures/extras.sql"},
		{Path: "fixtures/base.sql"},
	}
	rep2 := prepare.InspectFingerprint(env.Ctx, env.Store, swappedCfg, wt, o.SourceDB, "")
	if rep1.Fingerprint == rep2.Fingerprint {
		t.Errorf("reordering dumps must change the fingerprint (rep1=%s rep2=%s)",
			rep1.Fingerprint[:12], rep2.Fingerprint[:12])
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestPostgresFanoutConcurrency probes whether the Postgres clone
// fan-out actually runs in parallel server-side. `CREATE DATABASE …
// TEMPLATE` requires no connections to the template AND takes locks
// on pg_database, so multiple concurrent restores from the same
// template may serialize regardless of the outer goroutine count.
//
// The probe runs a cold build (to populate a template), then asserts
// the cache-hit fan-out into N clones produces some real overlap:
//
//	parallelism = sum(per-clone duration) / wall-clock duration
//
// 1.0 means perfectly serial, N means perfectly parallel. We require
// at least 1.5 — a weak lower bound that catches accidental full
// serialisation without flaking on shared-CI noise. The actual factor
// is logged so a future tightening / cap reduction in fanOutLimits
// has concrete data to lean on.
func TestPostgresFanoutConcurrency(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	cfg := buildConfig()
	// 8 clones: large enough to expose serialisation without
	// overwhelming the dev box's max_connections (~100).
	cfg.Databases[0].TestClones = &config.TestClonesSpec{
		Clones:       config.ClonesSetting{Fixed: 8},
		NameTemplate: "tm_pgcc_{slug}_w{n}",
	}
	env := harness.NewEnv(t, wt)

	// Cold build: creates the template + first fan-out. Re-running with
	// the same inputs hits the cache and exercises the parallel
	// SnapshotRestore path we want to measure.
	_ = harness.AssertOutcome(t, env.RunPrepare(t, cfg), "postgres", false)

	// Only count events emitted from THIS point forward — the prior cold
	// build also produced clone_restore_done events, and including those
	// would double-count and inflate the parallelism factor.
	probeStartMs := time.Now().UnixMilli()
	wallStart := time.Now()
	o2 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "postgres", true)
	wallMs := time.Since(wallStart).Milliseconds()
	if got := len(o2.Clones); got < 8 {
		t.Fatalf("expected ≥8 clones in fan-out, got %d", got)
	}

	// Pull per-clone restore durations from the event stream. Each
	// clone_restore_done event carries the clone's wall-clock duration
	// in DurationMs.
	evs, err := env.Store.QueryEvents(env.Ctx, store.EventFilter{
		WorktreeID: env.WTID,
		EventTypes: []string{"clone_restore_done"},
		SinceMs:    probeStartMs,
	})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("no clone_restore_done events recorded — fanout instrumentation regression")
	}
	var sumDur int64
	for _, e := range evs {
		if e.DurationMs.Valid {
			sumDur += e.DurationMs.Int64
		}
	}
	if wallMs <= 0 {
		t.Fatalf("wall duration not measured (got %dms)", wallMs)
	}
	parallelism := float64(sumDur) / float64(wallMs)
	t.Logf("postgres fan-out: clones=%d wall=%dms sum=%dms parallelism=%.2f",
		len(evs), wallMs, sumDur, parallelism)
	if parallelism < 1.5 {
		t.Errorf("parallelism factor %.2f < 1.5 — postgres fan-out appears to be serialising "+
			"(consider lowering fanOutLimits[postgres] in internal/prepare/prepare.go)",
			parallelism)
	}
}

// TestIncrementalAncestorBuild proves task #4 wired to postgres: adding
// a new migration on top of an already-cached template builds
// incrementally from the cached ancestor (clone via CREATE DATABASE
// TEMPLATE + migrate the new file only) instead of cold-rebuilding from
// the dump + replaying every migration. The fixture migrate.sh is
// ledger-aware (`_treeman_e2e_migrations`) so cloning the ancestor
// carries its partial ledger forward and only the new file actually
// runs.
func TestIncrementalAncestorBuild(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	env := harness.NewEnv(t, wt)

	o1 := harness.AssertOutcome(t, env.RunPrepare(t, buildConfig()), "postgres", false)
	if o1.IncrementalBase != "" {
		t.Fatalf("pass 1 (first cold build) should not be incremental; got base=%s", o1.IncrementalBase)
	}
	t.Logf("pass1 cold: fp=%s tmpl=%s", o1.Fingerprint[:12], o1.TemplateName)

	addMigration(t, wt)

	o2 := harness.AssertOutcome(t, env.RunPrepare(t, buildConfig()), "postgres", false)
	if o2.IncrementalBase != o1.Fingerprint {
		t.Errorf("pass 2 should build incrementally from pass 1; got IncrementalBase=%q want %q",
			o2.IncrementalBase, o1.Fingerprint)
	}
	if o2.Fingerprint == o1.Fingerprint {
		t.Errorf("pass 2 must produce a NEW fingerprint (extra migration): still %s", o1.Fingerprint[:12])
	}
	t.Logf("pass2 incremental: fp=%s tmpl=%s base=%s",
		o2.Fingerprint[:12], o2.TemplateName, o2.IncrementalBase[:12])

	assertTables(t, "127.0.0.1:15432", o2.SourceDB, []string{"products", "orders", "shipments"})
}

func assertWidgetCount(t *testing.T, addr, dbName string, want int) {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/%s?sslmode=disable", addr, dbName)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM widgets").Scan(&n); err != nil {
		t.Fatalf("count widgets in %s: %v", dbName, err)
	}
	if n != want {
		t.Errorf("widgets count in %s = %d, want %d", dbName, n, want)
	}
}

// pgDBExists reports whether a database named `name` lives in the
// cluster — checked from the `postgres` maintenance DB so it works even
// after `name` has been dropped.
func pgDBExists(t *testing.T, addr, name string) bool {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/postgres?sslmode=disable", addr)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg maintenance db: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n); err != nil {
		t.Fatalf("pg exists %s: %v", name, err)
	}
	return n == 1
}

func buildConfig() *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{
				Host:     "127.0.0.1",
				Port:     15432,
				User:     "postgres",
				Password: "pgpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "postgres",
				NameTemplate: "treeman_e2e_{slug}",
				Dump:         config.DumpList{{Path: "fixtures/seed.sql"}},
				Migrate: &config.Step{
					Run: "./fixtures/migrate.sh",
					Env: map[string]string{
						"DB_DATABASE":  "{target_db}",
						"PG_CONTAINER": "treeman-e2e-postgres",
					},
				},
				Inputs: []config.Input{
					{Glob: "fixtures/migrations/*.sql", Label: "migrations"},
				},
				// Fanout: pre-warm 2 paratest clones so teardown's
				// per-worktree cleanup (source + clones) is exercised.
				TestClones: &config.TestClonesSpec{
					Clones:       config.ClonesSetting{Fixed: 2},
					NameTemplate: "treeman_e2e_{slug}_w{n}",
				},
			},
		},
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyTree(t, s, d)
			continue
		}
		body, _ := os.ReadFile(s)
		info, _ := e.Info()
		mode := os.FileMode(0o644)
		if info != nil {
			mode = info.Mode().Perm()
		}
		_ = os.WriteFile(d, body, mode)
	}
}

func addMigration(t *testing.T, wt string) {
	body := `CREATE TABLE shipments (id SERIAL PRIMARY KEY, order_id INT NOT NULL REFERENCES orders(id));` + "\n"
	if err := os.WriteFile(filepath.Join(wt, "fixtures/migrations/002_shipments.sql"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertTables(t *testing.T, addr, dbName string, want []string) {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/%s?sslmode=disable", addr, dbName)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	got := map[string]bool{}
	rows, err := db.QueryContext(context.Background(),
		"SELECT tablename FROM pg_tables WHERE schemaname = 'public'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("table %q missing from %s (have: %v)", w, dbName, keys(got))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
