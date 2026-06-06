//go:build e2e

// Optimization-path e2e for the branch_scoped swap lifecycle, against a
// REAL MySQL: the lever-2 migrate gate (skip a redundant migrate on an
// unchanged resume) and the lever-1 capture gate (skip a redundant
// capture on a clean bounce, using the live InnoDB write watermark).
//
// These exercise the parts that the in-memory branchstate unit tests
// cannot: the real WriteWatermark query and the migrateFP fingerprint
// computed by prepare.Run from on-disk inputs.
package branchscoped_e2e

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// dropPg best-effort drops a database from the maintenance DB. WITH
// (FORCE) (pg 13+) evicts lingering sessions so cleanup can't hang.
func dropPg(t *testing.T, name string) {
	t.Helper()
	db := pgConn(t, "postgres")
	defer db.Close()
	if _, err := db.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
		t.Logf("drop pg %s: %v", name, err)
	}
}

// countEvents returns how many events of `typ` were recorded for a
// worktree — used to assert migrate/capture were (or weren't) skipped.
func countEvents(t *testing.T, st *store.Store, wtID int64, typ string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE event_type = ? AND worktree_id = ?`,
		typ, wtID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// countLines counts newline-terminated lines in a file, treating a
// missing file as zero — the migrate command appends one line per run.
func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// TestMigrateGateMySQL: a no-op migrate (append a line to a counter file)
// runs on the fresh build, is SKIPPED on an unchanged resume, and runs
// again once a new migration file flips the input fingerprint.
func TestMigrateGateMySQL(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMySQL(t)

	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)

	migDir := filepath.Join(wtPath, "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(migDir, "0001.sql"), []byte("-- v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(wtPath, "migrate.count")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13390, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "tm_bsmg_{slug}",
			BranchScoped: true,
			Inputs:       []config.Input{{Glob: "migrations/*.sql", Label: "migrations"}},
			Migrate:      &config.Step{Run: "echo x >> " + counter},
		}},
	}

	// Fresh build: active created empty → migrate runs once, no skip.
	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	t.Cleanup(func() { dropMySQL(t, active) })
	if got := countLines(t, counter); got != 1 {
		t.Fatalf("fresh build must migrate once, ran %d times", got)
	}
	if got := countEvents(t, st, wtID, "migrate:skip"); got != 0 {
		t.Fatalf("fresh build must not skip, got %d skips", got)
	}

	// Unchanged resume: same branch, same inputs → migrate skipped.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	if got := countLines(t, counter); got != 1 {
		t.Fatalf("unchanged resume must skip migrate, ran %d times total", got)
	}
	if got := countEvents(t, st, wtID, "migrate:skip"); got != 1 {
		t.Fatalf("unchanged resume must emit one migrate_skip, got %d", got)
	}

	// A new migration flips the fingerprint → migrate runs again.
	if err := os.WriteFile(filepath.Join(migDir, "0002.sql"), []byte("-- v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	if got := countLines(t, counter); got != 2 {
		t.Fatalf("changed inputs must re-migrate, ran %d times total", got)
	}
	if got := countEvents(t, st, wtID, "migrate:skip"); got != 1 {
		t.Fatalf("changed inputs must not add a skip, got %d", got)
	}
}

// TestCaptureGateMySQL: after a clean resume the outgoing capture is
// skipped (live write watermark unchanged), but a write before the next
// switch forces the capture — and that capture preserves the new row.
// On MySQL 8.x the watermark is the per-database performance_schema
// write count; isolation from sibling databases is covered by
// TestMultiWorktreeCaptureGateMySQL.
func TestCaptureGateMySQL(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMySQL(t)

	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13390, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "tm_bscap_{slug}",
			BranchScoped: true,
		}},
	}

	// develop: fresh empty, seed schema + a row.
	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	t.Cleanup(func() { dropMySQL(t, active) })
	mustExec(t, active, "CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32))")
	mustExec(t, active, "INSERT INTO items(v) VALUES('develop')")

	// feature (new) → branch point (develop's data), then diverge.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	assertItems(t, active, "develop")
	mustExec(t, active, "INSERT INTO items(v) VALUES('feature')")

	// back to develop → resume develop; this records the clean watermark.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	assertItems(t, active, "develop")

	// Clean bounce: switch away with NO write since the resume → the
	// capture of develop is redundant and must be skipped.
	skips := countEvents(t, st, wtID, "capture_skip")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	assertItems(t, active, "develop", "feature") // feature resumed intact
	if got := countEvents(t, st, wtID, "capture_skip"); got != skips+1 {
		t.Fatalf("clean bounce must skip a capture: capture_skip %d → %d (want +1)", skips, got)
	}

	// Resume develop again, then WRITE before switching → the watermark
	// advances, so the next switch MUST capture (no skip).
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	assertItems(t, active, "develop")
	mustExec(t, active, "INSERT INTO items(v) VALUES('extra')")
	skips = countEvents(t, st, wtID, "capture_skip")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	if got := countEvents(t, st, wtID, "capture_skip"); got != skips {
		t.Fatalf("a write before switch must force a capture: capture_skip %d → %d (want +0)", skips, got)
	}

	// The forced capture preserved 'extra' in develop's durable copy.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	assertItems(t, active, "develop", "extra")
}

// TestCaptureGatePostgres mirrors the MySQL capture-gate against real
// Postgres, whose watermark (pg_stat_database) is PER-DATABASE — so it is
// immune to writes against sibling databases, the cleanest of the sound
// signals.
func TestCaptureGatePostgres(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitPostgres(t)

	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{Host: "127.0.0.1", Port: 15490, User: "postgres", Password: "pgpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "postgres",
			NameTemplate: "tm_bscappg_{slug}",
			BranchScoped: true,
		}},
	}

	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	t.Cleanup(func() { dropPg(t, active) })
	pgExec(t, active, "CREATE TABLE items (id SERIAL PRIMARY KEY, v TEXT)")
	pgExec(t, active, "INSERT INTO items(v) VALUES('develop')")

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	pgAssertItems(t, active, "develop")
	pgExec(t, active, "INSERT INTO items(v) VALUES('feature')")

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop") // resume → clean watermark
	pgAssertItems(t, active, "develop")

	skips := countEvents(t, st, wtID, "capture_skip")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature") // clean bounce → skip
	pgAssertItems(t, active, "develop", "feature")
	if got := countEvents(t, st, wtID, "capture_skip"); got != skips+1 {
		t.Fatalf("clean bounce must skip a capture: capture_skip %d → %d (want +1)", skips, got)
	}

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	pgAssertItems(t, active, "develop")
	pgExec(t, active, "INSERT INTO items(v) VALUES('extra')") // dirties watermark
	skips = countEvents(t, st, wtID, "capture_skip")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	if got := countEvents(t, st, wtID, "capture_skip"); got != skips {
		t.Fatalf("a write before switch must force a capture: capture_skip %d → %d (want +0)", skips, got)
	}

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	pgAssertItems(t, active, "develop", "extra")
}

// TestCaptureGateElasticsearch mirrors the capture-gate against real
// Elasticsearch, whose watermark (_stats/indexing index_total+delete_total)
// is per-prefix and sound.
func TestCaptureGateElasticsearch(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitES(t)

	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Elasticsearch: &config.EsConn{URL: esURL},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "elasticsearch",
			KeyPrefix:    "bscapes_{slug}_",
			BranchScoped: true,
		}},
	}

	prefix := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	t.Cleanup(func() {
		_, _ = http.Post(
			esURL+"/"+prefix+"*/_delete_by_query?refresh=true",
			"application/json",
			strings.NewReader(`{"query":{"match_all":{}}}`),
		)
	})
	esIndex(t, prefix, "a", "develop")
	esAssertVals(t, prefix, map[string]string{"a": "develop"})

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	esAssertVals(t, prefix, map[string]string{"a": "develop"})
	esIndex(t, prefix, "b", "feature")

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop") // resume → clean watermark
	esAssertVals(t, prefix, map[string]string{"a": "develop"})

	skips := countEvents(t, st, wtID, "capture_skip")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature") // clean bounce → skip
	esAssertVals(t, prefix, map[string]string{"a": "develop", "b": "feature"})
	if got := countEvents(t, st, wtID, "capture_skip"); got != skips+1 {
		t.Fatalf("clean bounce must skip a capture: capture_skip %d → %d (want +1)", skips, got)
	}

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	esAssertVals(t, prefix, map[string]string{"a": "develop"})
	esIndex(t, prefix, "c", "extra") // dirties the indexing watermark
	skips = countEvents(t, st, wtID, "capture_skip")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	if got := countEvents(t, st, wtID, "capture_skip"); got != skips {
		t.Fatalf("a write before switch must force a capture: capture_skip %d → %d (want +0)", skips, got)
	}

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	esAssertVals(t, prefix, map[string]string{"a": "develop", "c": "extra"})
}

// TestCaptureNeverSkippedMongo locks the soundness floor: mongo exposes no
// sound cheap watermark (Watermark→""), so a capture must NEVER be skipped
// even on an otherwise-clean bounce — and isolation must still hold.
func TestCaptureNeverSkippedMongo(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMongo(t)

	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mongodb: &config.MongoConn{URI: mongoURI},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mongodb",
			NameTemplate: "tm_bscapmongo_{slug}",
			BranchScoped: true,
		}},
	}

	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	mongoInsert(t, active, "develop")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	mongoInsert(t, active, "feature")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	mongoAssert(t, active, "develop")
	// Clean bounce — but no watermark, so capture must still run.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	mongoAssert(t, active, "develop", "feature")
	if got := countEvents(t, st, wtID, "capture_skip"); got != 0 {
		t.Fatalf("mongo has no watermark; capture must never be skipped, got %d capture_skip", got)
	}
}

// TestMultiWorktreeCaptureGatePostgres proves the capture gate is
// partitioned per worktree: two sibling worktrees of the SAME repo get
// distinct active databases, independent clean/watermark bookkeeping, and
// a write in one worktree never dirties the other's gate. Postgres'
// per-database watermark makes this deterministic — the headline
// "several worktrees stay isolated" guarantee for lever 1.
func TestMultiWorktreeCaptureGatePostgres(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitPostgres(t)
	ctx := context.Background()

	st := openStore(t)
	root := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, root, "multi")
	if err != nil {
		t.Fatal(err)
	}
	wtA := filepath.Join(root, "a")
	wtB := filepath.Join(root, "b")
	for _, p := range []string{wtA, wtB} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wtIDA, err := st.EnsureWorktree(ctx, repoID, wtA, slug.For(wtA, "").Value, "develop")
	if err != nil {
		t.Fatal(err)
	}
	wtIDB, err := st.EnsureWorktree(ctx, repoID, wtB, slug.For(wtB, "").Value, "develop")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{Host: "127.0.0.1", Port: 15490, User: "postgres", Password: "pgpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "postgres",
			NameTemplate: "tm_bsmw_{slug}",
			BranchScoped: true,
		}},
	}

	// Drive one worktree to a clean develop resume with a divergent feature.
	bring := func(wtPath string, wtID int64) string {
		active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
		pgExec(t, active, "CREATE TABLE items (id SERIAL PRIMARY KEY, v TEXT)")
		pgExec(t, active, "INSERT INTO items(v) VALUES('"+filepath.Base(wtPath)+"')")
		drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
		pgExec(t, active, "INSERT INTO items(v) VALUES('feat-"+filepath.Base(wtPath)+"')")
		drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop") // resume → clean
		return active
	}

	activeA := bring(wtA, wtIDA)
	activeB := bring(wtB, wtIDB)
	t.Cleanup(func() { dropPg(t, activeA); dropPg(t, activeB) })

	if activeA == activeB {
		t.Fatalf("sibling worktrees must get distinct active databases, both = %s", activeA)
	}

	skipsA := countEvents(t, st, wtIDA, "capture_skip")

	// Write into B's active DB. B's per-database watermark advances; A's
	// must NOT — so A's next switch can still skip.
	pgExec(t, activeB, "INSERT INTO items(v) VALUES('bnoise')")

	// A clean bounce: must still skip despite the concurrent write in B.
	drivePrepare(t, st, cfg, wtA, repoID, wtIDA, "feature")
	if got := countEvents(t, st, wtIDA, "capture_skip"); got != skipsA+1 {
		t.Fatalf("worktree A's gate must be isolated from B's writes: capture_skip %d → %d (want +1)", skipsA, got)
	}

	// Data isolation: A resumes only A's rows; B kept its own + the noise.
	drivePrepare(t, st, cfg, wtA, repoID, wtIDA, "develop")
	pgAssertItems(t, activeA, "a")
	pgAssertItems(t, activeB, "bnoise", "b")
}

// TestMultiWorktreeCaptureGateMySQL proves the per-database MySQL
// watermark (performance_schema, enabled by default on 8.x) isolates
// sibling worktrees: a write into one worktree's active database must NOT
// dirty another's capture gate. Before the per-db watermark this could
// not hold — the server-wide Innodb_rows_* counter saw every database's
// writes — so this is the regression guard for that upgrade.
func TestMultiWorktreeCaptureGateMySQL(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMySQL(t)
	ctx := context.Background()

	st := openStore(t)
	root := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, root, "multimysql")
	if err != nil {
		t.Fatal(err)
	}
	wtA := filepath.Join(root, "a")
	wtB := filepath.Join(root, "b")
	for _, p := range []string{wtA, wtB} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wtIDA, err := st.EnsureWorktree(ctx, repoID, wtA, slug.For(wtA, "").Value, "develop")
	if err != nil {
		t.Fatal(err)
	}
	wtIDB, err := st.EnsureWorktree(ctx, repoID, wtB, slug.For(wtB, "").Value, "develop")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13390, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "tm_bsmwmy_{slug}",
			BranchScoped: true,
		}},
	}

	bring := func(wtPath string, wtID int64) string {
		active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
		mustExec(t, active, "CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32))")
		mustExec(t, active, "INSERT INTO items(v) VALUES('"+filepath.Base(wtPath)+"')")
		drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
		mustExec(t, active, "INSERT INTO items(v) VALUES('feat-"+filepath.Base(wtPath)+"')")
		drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop") // resume → clean
		return active
	}

	activeA := bring(wtA, wtIDA)
	activeB := bring(wtB, wtIDB)
	t.Cleanup(func() { dropMySQL(t, activeA); dropMySQL(t, activeB) })

	if activeA == activeB {
		t.Fatalf("sibling worktrees must get distinct active databases, both = %s", activeA)
	}

	skipsA := countEvents(t, st, wtIDA, "capture_skip")

	// Write into B's active DB. With a per-database watermark, A's gate
	// must be unaffected — so A's next switch still skips.
	mustExec(t, activeB, "INSERT INTO items(v) VALUES('bnoise')")

	drivePrepare(t, st, cfg, wtA, repoID, wtIDA, "feature")
	if got := countEvents(t, st, wtIDA, "capture_skip"); got != skipsA+1 {
		t.Fatalf("per-db watermark must isolate A from B's write: capture_skip %d → %d (want +1)", skipsA, got)
	}

	drivePrepare(t, st, cfg, wtA, repoID, wtIDA, "develop")
	assertItems(t, activeA, "a")
	assertItems(t, activeB, "bnoise", "b")
}

// TestMySQLWatermarkSourceSelection covers the watermark SOURCE choice
// against a real server — the part the capture-gate tests can't, since
// MySQL 8.x always has performance_schema on:
//
//  1. instrument enabled  → per-database token ("ps:")
//  2. instrument disabled → fall back to the global counter ("ir:"),
//     NOT a falsely-clean pinned-0 performance_schema read. This is the
//     soundness guard for the per-db watermark.
func TestMySQLWatermarkSourceSelection(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMySQL(t)
	ctx := context.Background()

	drv, err := dbmysql.Connect(ctx, config.MysqlConn{
		Host: "127.0.0.1", Port: 13390, User: "root", Password: "rootpw",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drv.Close() }()

	// performance_schema on by default → per-database token.
	wm, err := drv.WriteWatermark(ctx, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(wm, "ps:") {
		t.Fatalf("instrument enabled must use the per-db token, got %q", wm)
	}

	// Disable the table-io instrument. COUNT_WRITE then pins at 0, which
	// would read falsely "clean" — the guard must instead fall back to
	// the always-on global counter.
	if _, err := drv.DB.ExecContext(ctx,
		`UPDATE performance_schema.setup_instruments SET ENABLED='NO'
		 WHERE NAME='wait/io/table/sql/handler'`); err != nil {
		t.Fatalf("disable instrument: %v", err)
	}
	t.Cleanup(func() {
		_, _ = drv.DB.ExecContext(context.Background(),
			`UPDATE performance_schema.setup_instruments SET ENABLED='YES'
			 WHERE NAME='wait/io/table/sql/handler'`)
	})

	wm2, err := drv.WriteWatermark(ctx, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(wm2, "ir:") {
		t.Fatalf("instrument disabled must fall back to the global counter, got %q", wm2)
	}
}

// TestCaptureNeverSkippedRedis: same soundness floor for redis.
func TestCaptureNeverSkippedRedis(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitRedis(t)

	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{URL: "redis://" + redisAddr},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "redis",
			KeyPrefix:    "bscapr:{slug}:",
			BranchScoped: true,
		}},
	}

	prefix := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	rset(t, prefix+"a", "develop")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	rset(t, prefix+"b", "feature")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	assertRedisVals(t, prefix, map[string]string{"a": "develop"})
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	assertRedisVals(t, prefix, map[string]string{"a": "develop", "b": "feature"})
	if got := countEvents(t, st, wtID, "capture_skip"); got != 0 {
		t.Fatalf("redis has no watermark; capture must never be skipped, got %d capture_skip", got)
	}
}
