//go:build e2e

// Package branchscoped_e2e drives the branch_scoped swap lifecycle
// against REAL engines (MySQL name-scoped, Redis prefix-scoped), for
// BOTH a linked worktree (active namespace fixed per path) and the main
// worktree (bare active namespace, swap fired by the real daemon HEAD
// watcher on `git checkout`).
//
// The invariant under test — the whole point of branch_scoped — is:
// manual data written while on branch A survives a round-trip to branch
// B and back, A's and B's divergent state stay isolated, and a brand-new
// branch starts from its branch point (the data that was live when it
// was cut).
//
//	develop: insert "develop"          → active = {develop}
//	switch feature (new)               → active = {develop}        (branch point)
//	feature: insert "feature"          → active = {develop,feature}
//	switch develop                     → active = {develop}        (feature isolated)
//	switch feature                     → active = {develop,feature}(feature resumed)
package branchscoped_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	redis "github.com/redis/go-redis/v9"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

const (
	mysqlAddr = "127.0.0.1:13390"
	redisAddr = "127.0.0.1:16390"
)

// ─── TestWorktreeSwapMySQL: linked worktree, name-scoped engine ──────

func TestWorktreeSwapMySQL(t *testing.T) {
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
			NameTemplate: "tm_bs_{slug}",
			BranchScoped: true,
		}},
	}

	// develop: fresh empty active → seed schema + a row.
	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	mustExec(t, active, "CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32))")
	mustExec(t, active, "INSERT INTO items(v) VALUES('develop')")

	// switch to a brand-new branch → starts from develop's data (branch point).
	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != active {
		t.Fatalf("active namespace drifted across switch: %s → %s", active, got)
	}
	assertItems(t, active, "develop")

	// diverge feature.
	mustExec(t, active, "INSERT INTO items(v) VALUES('feature')")
	assertItems(t, active, "develop", "feature")

	// back to develop → feature's divergence isolated, develop restored.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	assertItems(t, active, "develop")

	// back to feature → feature's divergence resumed.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	assertItems(t, active, "develop", "feature")
}

// ─── TestWorktreeSwapRedis: linked worktree, prefix-scoped engine ────

func TestWorktreeSwapRedis(t *testing.T) {
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
			KeyPrefix:    "bs:{slug}:",
			BranchScoped: true,
		}},
	}

	prefix := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	rset(t, prefix+"a", "develop")
	assertRedisVals(t, prefix, map[string]string{"a": "develop"})

	// new branch → branch point (develop's keys).
	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != prefix {
		t.Fatalf("active prefix drifted: %s → %s", prefix, got)
	}
	assertRedisVals(t, prefix, map[string]string{"a": "develop"})

	rset(t, prefix+"b", "feature")
	assertRedisVals(t, prefix, map[string]string{"a": "develop", "b": "feature"})

	// back to develop → feature key isolated.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	assertRedisVals(t, prefix, map[string]string{"a": "develop"})

	// back to feature → resumed.
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	assertRedisVals(t, prefix, map[string]string{"a": "develop", "b": "feature"})
}

// ─── TestTeardownPreservesBranchData: capture-before-drop on delete ─
//
// Locks the data-loss invariant for `wt delete` on a branch_scoped
// database: teardown MUST capture the active namespace into the
// current branch's durable copy BEFORE dropping the active. A
// re-created worktree at the same path on the same branch must then
// resume the diverged data instead of starting blank.
//
// A regression here would silently destroy every row of branch-local
// divergent state on the first `wt delete`, so the assertion is split:
//
//  1. The active namespace is gone after teardown (drop happened).
//  2. A `_tmbs_*` durable database appeared during teardown (capture
//     happened — distinguishes "drop without capture" from the
//     correct "capture then drop").
//  3. Re-running prepare at the same path restores the row (the
//     durable holds the data, not just metadata).
func TestTeardownPreservesBranchData(t *testing.T) {
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
			NameTemplate: "tm_bstd_{slug}",
			BranchScoped: true,
		}},
	}

	// develop: seed schema + a sentinel row.
	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	t.Cleanup(func() { dropMySQL(t, active) })
	mustExec(t, active, "CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32))")
	mustExec(t, active, "INSERT INTO items(v) VALUES('preserved')")

	// Snapshot existing _tmbs_* databases so the post-teardown diff
	// isolates the durable copy created by THIS teardown.
	before := listMySQLDBsWithPrefix(t, "_tmbs_")

	// Simulate `wt delete`: TeardownDatabases is what wt/delete.go runs.
	sl := slug.For(wtPath, "")
	if err := prepare.TeardownDatabases(context.Background(), cfg, sl.Value, repoID, wtID, st); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if mysqlDBExists(t, active) {
		t.Fatalf("teardown must drop active %q", active)
	}
	after := listMySQLDBsWithPrefix(t, "_tmbs_")
	newDurables := diffStrings(after, before)
	if len(newDurables) == 0 {
		t.Fatal("teardown must capture active into a durable copy BEFORE dropping it; no new _tmbs_* database appeared")
	}
	t.Cleanup(func() {
		for _, d := range newDurables {
			dropMySQL(t, d)
		}
	})

	// Simulate `wt create` at the same path on the same branch: prepare
	// must resume from the durable copy, restoring the row.
	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop"); got != active {
		t.Fatalf("active namespace drifted across teardown+recreate: %s → %s", active, got)
	}
	assertItems(t, active, "preserved")
}

// TestTeardownNoOpWhenNeverPrepared: teardownBranchScoped must exit
// cleanly when the worktree's active namespace was never created (e.g.
// `wt create` failed before prepare ran). No active to drop, no marker
// to clear, no error.
func TestTeardownNoOpWhenNeverPrepared(t *testing.T) {
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
			NameTemplate: "tm_bstd_noop_{slug}",
			BranchScoped: true,
		}},
	}

	sl := slug.For(wtPath, "")
	if err := prepare.TeardownDatabases(context.Background(), cfg, sl.Value, repoID, wtID, st); err != nil {
		t.Fatalf("teardown on never-prepared worktree must be a no-op, got %v", err)
	}
}

// ─── TestMainSwapViaDaemon: main worktree, real HEAD watcher ─────────
//
// Proves the full main-worktree path: enroll the repo root, attach the
// real HEAD watcher, and `git checkout` to fire the swap through the
// daemon's FinalizeWorktree. The active namespace is the bare overlay
// name the app's `.env` points at — it never changes; only its contents
// swap.
func TestMainSwapViaDaemon(t *testing.T) {
	harness.SkipIfNoDocker(t)
	requireGit(t)
	waitMySQL(t)

	repoRoot := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "develop", repoRoot)
	mustGit(t, repoRoot, "config", "user.email", "t@t")
	mustGit(t, repoRoot, "config", "user.name", "t")
	// README guarantees the commit has a tracked file even when a global
	// gitignore excludes .treeman.yaml (treeman reads it from disk, so it
	// need not be committed).
	writeFile(t, repoRoot, "README", "hi")
	yaml := `
main_worktree:
  enabled: true
  databases:
    - name_template: "tm_bsmain_app"
connections:
  mysql:
    host: 127.0.0.1
    port: 13390
    user: root
    password: rootpw
databases:
  - engine: mysql
    name_template: "tm_bsmain_{slug}"
    branch_scoped: true
`
	writeFile(t, repoRoot, ".treeman.yaml", yaml)
	mustGit(t, repoRoot, "add", "-A")
	mustGit(t, repoRoot, "commit", "-q", "-m", "init")

	const active = "tm_bsmain_app"
	dropMySQL(t, active) // clean slate across reruns

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // registered first → runs LAST
	env := map[string]string{"PATH": os.Getenv("PATH")}

	// The HEAD watcher's finalize goroutines run on the State's
	// background context. Use a cancellable one (not context.Background)
	// so cleanup can stop the watcher and drain any in-flight finalize
	// BEFORE the store closes — otherwise a debounced swap fires against a
	// closed DB ("sql: database is closed") and leaks a goroutine into the
	// next test.
	daemonCtx, cancelDaemon := context.WithCancel(ctx)
	state := daemon.NewState(daemonCtx, st)
	t.Cleanup(func() { // registered after st.Close → runs FIRST
		cancelDaemon()
		_ = state.WaitFinalizeCleared(context.Background(), repoRoot, 10*time.Second)
	})
	if _, err := daemon.EnrollMainWorktree(ctx, state, repoRoot); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// Seed the active DB on develop (what `treeman main enable` does).
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env, false); err != nil {
		t.Fatalf("finalize develop: %v", err)
	}
	waitForDB(t, active, 30*time.Second)
	mustExec(t, active, "CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32))")
	mustExec(t, active, "INSERT INTO items(v) VALUES('develop')")

	// Resolve the main worktree row so we can poll the active-branch
	// marker — it flips only AFTER a swap completes, which is the
	// reliable signal that the HEAD-triggered finalize finished (the DB
	// contents alone are ambiguous: the branch-point state can equal the
	// pre-swap state).
	repoID, err := st.LookupRepoID(ctx, repoRoot)
	if err != nil || repoID == 0 {
		t.Fatalf("lookup repo: %v", err)
	}
	mainRow, err := st.LookupMainWorktree(ctx, repoID)
	if err != nil || mainRow.ID == 0 {
		t.Fatalf("lookup main worktree: %v", err)
	}
	mainWtID := mainRow.ID

	if err := daemon.ResumeWorktreeWatcher(ctx, state, repoRoot, repoRoot); err != nil {
		t.Fatalf("resume wt watcher: %v", err)
	}
	time.Sleep(700 * time.Millisecond) // let HEAD watcher seed lastSeen

	// Real checkout → HEAD watcher → finalize → swap. New branch starts
	// from develop's data (branch point). Wait for the swap to land
	// (marker flips to feature/x) before mutating data.
	mustGit(t, repoRoot, "checkout", "-q", "-b", "feature/x")
	waitSwap(t, st, state, mainWtID, active, repoRoot, "feature/x")
	assertItems(t, active, "develop")

	mustExec(t, active, "INSERT INTO items(v) VALUES('feature')")

	// Back to develop → feature isolated, develop restored.
	mustGit(t, repoRoot, "checkout", "-q", "develop")
	waitSwap(t, st, state, mainWtID, active, repoRoot, "develop")
	assertItems(t, active, "develop")

	// Forward to feature again → feature resumed.
	mustGit(t, repoRoot, "checkout", "-q", "feature/x")
	waitSwap(t, st, state, mainWtID, active, repoRoot, "feature/x")
	assertItems(t, active, "develop", "feature")
}

// waitSwap blocks until the HEAD-triggered swap to `branch` has FULLY
// completed, not merely started. Two gates:
//
//  1. the active-branch marker flips to `branch` — the swap captured the
//     old branch's data and recorded the new owner; and
//  2. the finalize clears — fill/restore (and any migrate) finished.
//
// Gate 1 alone is insufficient: runBranchScoped sets the marker BEFORE it
// fills the active namespace (so a crash can't re-capture), so asserting
// data right after the marker races the restore — the source of this
// test's flake under load. Draining the in-flight finalize also keeps the
// NEXT checkout from being deduped away by MarkFinalizeInFlight.
func waitSwap(t *testing.T, st *store.Store, state *daemon.State, wtID int64, active, wtPath, branch string) {
	t.Helper()
	waitForMarker(t, st, wtID, active, branch, 20*time.Second)
	if err := state.WaitFinalizeCleared(context.Background(), wtPath, 20*time.Second); err != nil {
		t.Fatalf("finalize did not clear after swap to %s: %v", branch, err)
	}
}

// waitForMarker polls the active-branch marker until it reads
// `wantBranch`, proving the swap-triggered finalize has completed.
func waitForMarker(t *testing.T, st *store.Store, wtID int64, active, wantBranch string, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		got, ok, err := st.GetActiveBranch(ctx, wtID, active)
		if err == nil && ok {
			last = got
			if got == wantBranch {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("active-branch marker for %s = %q, want %q within %s", active, last, wantBranch, timeout)
}

// ─── helpers ────────────────────────────────────────────────────────

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func registerWorktree(t *testing.T, st *store.Store, wtPath string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	repoID, err := st.EnsureRepo(ctx, wtPath, filepath.Base(wtPath))
	if err != nil {
		t.Fatal(err)
	}
	sl := slug.For(wtPath, "")
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, "develop")
	if err != nil {
		t.Fatal(err)
	}
	return repoID, wtID
}

// drivePrepare points the worktree row at `branch` (simulating a
// checkout), runs prepare, and returns the active namespace.
func drivePrepare(t *testing.T, st *store.Store, cfg *config.Config, wtPath string, repoID, wtID int64, branch string) string {
	t.Helper()
	ctx := context.Background()
	sl := slug.For(wtPath, "")
	if _, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, branch); err != nil {
		t.Fatal(err)
	}
	outs, err := prepare.Run(ctx, cfg, wtPath, sl, st, repoID, wtID, nil)
	if err != nil {
		t.Fatalf("prepare.Run(%s): %v", branch, err)
	}
	for _, o := range outs {
		if o.SourceDB != "" {
			return o.SourceDB
		}
	}
	t.Fatalf("prepare.Run(%s): no outcome with active namespace", branch)
	return ""
}

func mysqlDB(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root:rootpw@tcp(%s)/%s?multiStatements=true", mysqlAddr, dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	return db
}

func mustExec(t *testing.T, dbName, q string) {
	t.Helper()
	db := mysqlDB(t, dbName)
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec on %s: %q: %v", dbName, q, err)
	}
}

func dropMySQL(t *testing.T, dbName string) {
	t.Helper()
	db := mysqlDB(t, "")
	defer db.Close()
	if _, err := db.Exec("DROP DATABASE IF EXISTS " + dbName); err != nil {
		t.Fatalf("drop %s: %v", dbName, err)
	}
}

func itemsOf(t *testing.T, dbName string) ([]string, error) {
	db := mysqlDB(t, dbName)
	defer db.Close()
	rows, err := db.Query("SELECT v FROM items ORDER BY v")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func assertItems(t *testing.T, dbName string, want ...string) {
	t.Helper()
	got, err := itemsOf(t, dbName)
	if err != nil {
		t.Fatalf("read items from %s: %v", dbName, err)
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s items = %v, want %v", dbName, got, want)
	}
}

// listMySQLDBsWithPrefix returns every schema whose name starts with
// `prefix`. Uses LIKE with the SQL underscore wildcard escaped so a
// caller-passed `_tmbs_` matches literally instead of "any char".
func listMySQLDBsWithPrefix(t *testing.T, prefix string) []string {
	t.Helper()
	db := mysqlDB(t, "")
	defer db.Close()
	esc := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(prefix)
	rows, err := db.Query(`SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ? ESCAPE '\\'`, esc+"%")
	if err != nil {
		t.Fatalf("list schemas like %q: %v", prefix, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// diffStrings returns elements in `a` that are not in `b`.
func diffStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(b))
	for _, s := range b {
		seen[s] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := seen[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

func waitForDB(t *testing.T, dbName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		db := mysqlDB(t, "")
		var n int
		err := db.QueryRow("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", dbName).Scan(&n)
		db.Close()
		if err == nil && n == 1 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("database %s never appeared within %s", dbName, timeout)
}

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func rset(t *testing.T, key, val string) {
	t.Helper()
	if err := redisClient(t).Set(context.Background(), key, val, 0).Err(); err != nil {
		t.Fatalf("redis set %s: %v", key, err)
	}
}

func assertRedisVals(t *testing.T, prefix string, want map[string]string) {
	t.Helper()
	c := redisClient(t)
	ctx := context.Background()
	got := map[string]string{}
	iter := c.Scan(ctx, 0, prefix+"*", 100).Iterator()
	for iter.Next(ctx) {
		k := iter.Val()
		v, err := c.Get(ctx, k).Result()
		if err != nil {
			t.Fatalf("get %s: %v", k, err)
		}
		got[strings.TrimPrefix(k, prefix)] = v
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("scan %s*: %v", prefix, err)
	}
	if len(got) != len(want) {
		t.Fatalf("prefix %s: got %v, want %v", prefix, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("prefix %s key %q = %q, want %q (full: %v)", prefix, k, got[k], v, got)
		}
	}
}

func waitMySQL(t *testing.T) {
	harness.WaitForReady(t, "mysql:"+mysqlAddr, 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", mysqlAddr, time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		db := mysqlDB(t, "")
		defer db.Close()
		return db.Ping()
	})
}

func waitRedis(t *testing.T) {
	harness.WaitForReady(t, "redis:"+redisAddr, 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", redisAddr, time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}
