//go:build e2e

// Package snapshot_gc_e2e exercises the snapshots periodic-sweep policy
// knobs against a real MySQL + the live snapshot store:
//
//   - max_age_days   → SweepByAge drops templates older than the cutoff.
//   - max_total_gb   → SweepBySize evicts the largest templates until the
//     recorded total falls below the cap.
//   - keep_per_source → SweepBySource keeps the N most-recently-used
//     templates per source (migrations_hash) and evicts
//     the older ones.
//   - gc_interval_minutes → the daemon SnapshotGCLoop fires the sweep on
//     its configured cadence (forced to 1 minute here so
//     a tick lands inside the test).
//
// cap_per_repo (the inline-on-write eviction) is covered by e2e/retention.
//
// Each test opens a fresh snapshot store (isolated SQLite) but shares the
// MySQL server, so template DB names are uniquely prefixed per test.
package snapshot_gc_e2e

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
)

const (
	mysqlAddr = "127.0.0.1:13341"
	gib       = int64(1024 * 1024 * 1024)
)

func mysqlConn() config.MysqlConn {
	return config.MysqlConn{Host: "127.0.0.1", Port: 13341, User: "root", Password: "rootpw"}
}

// ─── TestSweepByAge: max_age_days ────────────────────────────────────

func TestSweepByAge(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bringUpMySQL(t)
	ctx := context.Background()

	st := openStore(t)
	drv := mysqlDriver(t)
	repoID := mustRepo(t, st, "age")

	now := time.Now()
	old := "_tm_gcage_old"
	fresh := "_tm_gcage_fresh"
	createTemplate(t, drv, st, repoID, old, now.Add(-10*24*time.Hour).UnixMilli(), 0)
	createTemplate(t, drv, st, repoID, fresh, now.UnixMilli(), 0)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Mysql: ptr(mysqlConn())},
		Snapshots:   config.SnapshotsConfig{MaxAgeDays: 7},
	}
	snapshot.SweepByAge(ctx, cfg, st)

	if dbExists(t, old) || rowExists(t, st, old) {
		t.Errorf("age sweep should have evicted %s (db=%v row=%v)", old, dbExists(t, old), rowExists(t, st, old))
	}
	if !dbExists(t, fresh) || !rowExists(t, st, fresh) {
		t.Errorf("age sweep wrongly evicted fresh template %s", fresh)
	}
}

// ─── TestSweepBySize: max_total_gb ───────────────────────────────────

func TestSweepBySize(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bringUpMySQL(t)
	ctx := context.Background()

	st := openStore(t)
	drv := mysqlDriver(t)
	repoID := mustRepo(t, st, "size")

	now := time.Now().UnixMilli()
	big := "_tm_gcsize_big"
	mid := "_tm_gcsize_mid"
	small := "_tm_gcsize_small"
	createTemplate(t, drv, st, repoID, big, now, 2*gib)
	createTemplate(t, drv, st, repoID, mid, now, 1*gib)
	createTemplate(t, drv, st, repoID, small, now, 256*1024*1024)

	// Cap 1 GiB. Total ≈ 3.25 GiB → evict largest-first (big, then mid)
	// until ≤ cap; small (0.25 GiB) survives.
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Mysql: ptr(mysqlConn())},
		Snapshots:   config.SnapshotsConfig{MaxTotalGb: 1},
	}
	snapshot.SweepBySize(ctx, cfg, st)

	if dbExists(t, big) || rowExists(t, st, big) {
		t.Errorf("size sweep should have evicted largest template %s", big)
	}
	if dbExists(t, mid) || rowExists(t, st, mid) {
		t.Errorf("size sweep should have evicted next-largest template %s", mid)
	}
	if !dbExists(t, small) || !rowExists(t, st, small) {
		t.Errorf("size sweep wrongly evicted %s (total was already under cap)", small)
	}
}

// ─── TestSweepBySource: keep_per_source ──────────────────────────────
//
// keep_per_source bounds how many templates survive per source (the
// migration-content key). With keep=1, a source that accumulated three
// templates (deps/dump churn while migrations held steady) keeps only
// its most-recently-used; the rest are evicted. A separate source is
// untouched.
func TestSweepBySource(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bringUpMySQL(t)
	ctx := context.Background()

	st := openStore(t)
	drv := mysqlDriver(t)
	repoID := mustRepo(t, st, "src")

	now := time.Now().UnixMilli()
	// Source alpha: 3 templates, newest last_used = now.
	aOld := "_tm_gcsrc_a_old"
	aMid := "_tm_gcsrc_a_mid"
	aNew := "_tm_gcsrc_a_new"
	seedTemplate(t, drv, st, repoID, aOld, "migA", now-3000, 0)
	seedTemplate(t, drv, st, repoID, aMid, "migA", now-2000, 0)
	seedTemplate(t, drv, st, repoID, aNew, "migA", now, 0)
	// Source beta: a single template — under any keep≥1.
	bOnly := "_tm_gcsrc_b_only"
	seedTemplate(t, drv, st, repoID, bOnly, "migB", now-1000, 0)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Mysql: ptr(mysqlConn())},
		Snapshots:   config.SnapshotsConfig{KeepPerSource: 1},
	}
	snapshot.SweepBySource(ctx, cfg, st)

	// Source alpha: only the newest survives.
	if !dbExists(t, aNew) || !rowExists(t, st, aNew) {
		t.Errorf("keep_per_source wrongly evicted the most-recent template %s", aNew)
	}
	for _, evicted := range []string{aOld, aMid} {
		if dbExists(t, evicted) || rowExists(t, st, evicted) {
			t.Errorf("keep_per_source should have evicted older template %s (db=%v row=%v)",
				evicted, dbExists(t, evicted), rowExists(t, st, evicted))
		}
	}
	// Source beta: untouched.
	if !dbExists(t, bOnly) || !rowExists(t, st, bOnly) {
		t.Errorf("keep_per_source evicted the only template of a distinct source %s", bOnly)
	}
}

// ─── TestGCLoopFiresOnInterval: gc_interval_minutes ──────────────────
//
// Drive the real daemon SnapshotGCLoop. A global config (under an
// isolated XDG_CONFIG_HOME) sets gc_interval_minutes: 1 + max_age_days:
// 1, so within a couple of ticks the loop's age sweep evicts an
// over-age template. Proves the cadence knob actually schedules the
// sweep end-to-end.
func TestGCLoopFiresOnInterval(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bringUpMySQL(t)

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	cfgDir := filepath.Join(xdg, "treeman")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	globalYAML := `connections:
  mysql:
    host: 127.0.0.1
    port: 13341
    user: root
    password: rootpw
snapshots:
  gc_interval_minutes: 1
  max_age_days: 1
`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(globalYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	st := openStore(t)
	drv := mysqlDriver(t)
	repoRoot := t.TempDir()
	repoID, err := st.EnsureRepo(context.Background(), repoRoot, filepath.Base(repoRoot))
	if err != nil {
		t.Fatal(err)
	}

	overAge := "_tm_gcloop_old"
	createTemplate(t, drv, st, repoID, overAge, time.Now().Add(-2*24*time.Hour).UnixMilli(), 0)

	loopCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go daemon.SnapshotGCLoop(loopCtx, daemon.NewState(loopCtx, st))

	// First tick lands at ~60s (1-minute cadence). Poll until the
	// over-age template is evicted, with headroom for a second tick.
	deadline := time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		if !dbExists(t, overAge) && !rowExists(t, st, overAge) {
			return // swept — the cadence knob fired the GC loop
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("GC loop did not evict over-age template %s within the interval window", overAge)
}

// ─── helpers ─────────────────────────────────────────────────────────

func bringUpMySQL(t *testing.T) {
	t.Helper()
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:"+mysqlAddr, 60*time.Second, func() error {
		db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mysqlDriver(t *testing.T) *dbmysql.Driver {
	t.Helper()
	drv, err := dbmysql.Connect(context.Background(), mysqlConn())
	if err != nil {
		t.Fatalf("mysql connect: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

func mustRepo(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	id, err := st.EnsureRepo(context.Background(), filepath.Join(t.TempDir(), name), name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// createTemplate CREATEs a real template database in MySQL and records
// its snapshot row with the given last-used timestamp + recorded size,
// so the GC sweeps have both engine-side and SQLite-side state to act on.
func createTemplate(t *testing.T, drv *dbmysql.Driver, st *store.Store, repoID int64, name string, lastUsedAt, sizeBytes int64) {
	t.Helper()
	seedTemplate(t, drv, st, repoID, name, "", lastUsedAt, sizeBytes)
}

// seedTemplate is createTemplate with an explicit source key
// (migrations_hash) — used by the keep_per_source sweep test where the
// grouping matters.
func seedTemplate(t *testing.T, drv *dbmysql.Driver, st *store.Store, repoID int64, name, migHash string, lastUsedAt, sizeBytes int64) {
	t.Helper()
	ctx := context.Background()
	if err := drv.EnsureDB(ctx, name); err != nil {
		t.Fatalf("create template db %s: %v", name, err)
	}
	t.Cleanup(func() { _ = drv.DropSnapshot(context.Background(), name) })
	if err := st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    name, // unique per template name
		Engine:         "mysql",
		SourceDB:       "src_" + name,
		TemplateName:   name,
		MigrationsHash: migHash,
		RepoID:         repoID,
		LastUsedAt:     lastUsedAt,
		SizeBytes:      sizeBytes,
	}); err != nil {
		t.Fatalf("record snapshot %s: %v", name, err)
	}
}

func dbExists(t *testing.T, name string) bool {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&n); err != nil {
		t.Fatalf("schema check %s: %v", name, err)
	}
	return n == 1
}

func rowExists(t *testing.T, st *store.Store, fingerprint string) bool {
	t.Helper()
	rec, err := st.LookupSnapshot(context.Background(), fingerprint)
	if err != nil {
		t.Fatalf("lookup snapshot %s: %v", fingerprint, err)
	}
	return rec != nil
}

func ptr[T any](v T) *T { return &v }
