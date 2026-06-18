//go:build e2e

// Engine coverage for the branch_scoped swap lifecycle beyond the
// MySQL + Redis cases in branchscoped_test.go: the remaining name-scoped
// engines (Postgres, MongoDB), the remaining prefix-scoped engine
// (Elasticsearch), plus the two lifecycle paths that the swap tests
// don't touch — `treeman db reset` (re-seed from the live parent) and
// `wt delete` teardown (capture durable, drop active, durable kept so a
// re-create resumes).
//
// All engine instances come up ONCE per package via TestMain; tests key
// every namespace off a unique t.TempDir slug so they stay isolated
// without per-test volume wipes.
package branchscoped_e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/template"
)

const (
	pgAddr   = "127.0.0.1:15490"
	mongoURI = "mongodb://127.0.0.1:27190"
	esURL    = "http://127.0.0.1:19290"
)

// TestMain brings the whole engine stack up once (when docker is
// available) and tears it down at the end. Individual tests still call
// harness.SkipIfNoDocker so a docker-less machine sees clean skips.
func TestMain(m *testing.M) {
	if !dockerReady() {
		os.Exit(m.Run())
	}
	dir := harness.MustAbs(".")
	if out, err := composeCmd(dir, "up", "-d", "--wait"); err != nil {
		fmt.Fprintf(os.Stderr, "branchscoped: compose up failed: %v\n%s\n", err, out)
		os.Exit(1)
	}
	code := m.Run()
	if out, err := composeCmd(dir, "down", "-v", "--remove-orphans"); err != nil {
		fmt.Fprintf(os.Stderr, "branchscoped: compose down: %v\n%s\n", err, out)
	}
	os.Exit(code)
}

func dockerReady() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "info").Run() == nil
}

func composeCmd(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// ─── TestWorktreeSwapPostgres: linked worktree, name-scoped engine ───

func TestWorktreeSwapPostgres(t *testing.T) {
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
			NameTemplate: "tm_bspg_{slug}",
			BranchScoped: true,
		}},
	}

	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	pgExec(t, active, "CREATE TABLE items (id SERIAL PRIMARY KEY, v TEXT)")
	pgExec(t, active, "INSERT INTO items(v) VALUES('develop')")

	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != active {
		t.Fatalf("active namespace drifted across switch: %s → %s", active, got)
	}
	pgAssertItems(t, active, "develop") // branch point

	pgExec(t, active, "INSERT INTO items(v) VALUES('feature')")
	pgAssertItems(t, active, "develop", "feature")

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	pgAssertItems(t, active, "develop") // feature isolated

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	pgAssertItems(t, active, "develop", "feature") // feature resumed
}

// ─── TestWorktreeSwapMongo: linked worktree, name-scoped engine ──────

func TestWorktreeSwapMongo(t *testing.T) {
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
			NameTemplate: "tm_bsmongo_{slug}",
			BranchScoped: true,
		}},
	}

	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	mongoInsert(t, active, "develop") // creates the db

	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != active {
		t.Fatalf("active namespace drifted: %s → %s", active, got)
	}
	mongoAssert(t, active, "develop") // branch point

	mongoInsert(t, active, "feature")
	mongoAssert(t, active, "develop", "feature")

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	mongoAssert(t, active, "develop") // feature isolated

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	mongoAssert(t, active, "develop", "feature") // resumed
}

// ─── TestWorktreeSwapElasticsearch: linked, prefix-scoped engine ─────

func TestWorktreeSwapElasticsearch(t *testing.T) {
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
			KeyPrefix:    "bses_{slug}_",
			BranchScoped: true,
		}},
	}

	prefix := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	esIndex(t, prefix, "a", "develop")
	esAssertVals(t, prefix, map[string]string{"a": "develop"})

	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != prefix {
		t.Fatalf("active prefix drifted: %s → %s", prefix, got)
	}
	esAssertVals(t, prefix, map[string]string{"a": "develop"}) // branch point

	esIndex(t, prefix, "b", "feature")
	esAssertVals(t, prefix, map[string]string{"a": "develop", "b": "feature"})

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	esAssertVals(t, prefix, map[string]string{"a": "develop"}) // feature isolated

	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	esAssertVals(t, prefix, map[string]string{"a": "develop", "b": "feature"}) // resumed
}

// ─── TestDbResetSparesSiblingPrefixElasticsearch ─────────────────────
//
// Prefix-engine analogue of TestDbResetDropsActiveExactNotPrefixMySQL.
// On the main worktree the branch_scoped active prefix is bare
// (overlay-resolved, slug-free), so it is a prefix of every linked
// worktree's `<prefix>_<slug>_*` indices. `db reset` must drop only what
// THIS worktree owns and spare the sibling's. Name-scoped engines solve
// this with an EXACT drop; ES has no exact drop, so the swap layer
// excludes any index carrying a sibling slug token. Without that filter
// DropMatching(bare) wipes the sibling's live data — the ES twin of the
// mysql exact-drop regression.
func TestDbResetSparesSiblingPrefixElasticsearch(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitES(t)
	ctx := context.Background()

	const activePrefix = "bsexactes_"          // bare main-wt active prefix
	const siblingPrefix = "bsexactes_feature_" // a linked wt's prefix — nests under the bare one
	const mainIndex = activePrefix + "items"
	const siblingIndex = siblingPrefix + "items"

	dropESIndex(t, mainIndex)
	dropESIndex(t, siblingIndex)
	t.Cleanup(func() { dropESIndex(t, mainIndex); dropESIndex(t, siblingIndex) })

	esIndex(t, activePrefix, "1", "main")     // → mainIndex
	esIndex(t, siblingPrefix, "1", "feature") // → siblingIndex

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Elasticsearch: &config.EsConn{URL: esURL},
		},
		// Bare key_prefix (no {slug}) models the main worktree's
		// overlay-resolved active prefix.
		Databases: []config.DatabaseConfig{{
			Engine:       "elasticsearch",
			KeyPrefix:    activePrefix,
			BranchScoped: true,
		}},
	}

	st := openStore(t)
	repoRoot := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		t.Fatal(err)
	}
	// Main worktree (runs the reset) + a sibling whose slug is "feature";
	// the sibling slug is what the swap layer uses to spare its indices.
	mainWtID, err := st.EnsureWorktree(ctx, repoID, repoRoot, "main", "develop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureWorktree(ctx, repoID, filepath.Join(repoRoot, "feature"), "feature", "feature"); err != nil {
		t.Fatal(err)
	}

	if err := prepare.ResetBranchScoped(ctx, cfg, repoRoot, repoID, mainWtID, st, ""); err != nil {
		t.Fatalf("ResetBranchScoped: %v", err)
	}

	if n := esCount(t, mainIndex+"*"); n != 0 {
		t.Fatalf("reset should have dropped the main worktree's own index %q, found %d docs", mainIndex, n)
	}
	if n := esCount(t, siblingIndex+"*"); n != 1 {
		t.Fatalf(
			"reset wiped sibling index %q via a bare-prefix match — sibling-isolation regression (found %d docs, want 1)",
			siblingIndex,
			n,
		)
	}
}

// ─── TestDbResetSparesSiblingPrefixRedis ─────────────────────────────
//
// Redis twin of TestDbResetSparesSiblingPrefixElasticsearch (and the
// prefix-engine analogue of TestDbResetDropsActiveExactNotPrefixMySQL).
// On the main worktree the branch_scoped active prefix is bare, so it is
// a prefix of every linked worktree's `<prefix>_<slug>_*` keys. `db reset`
// must drop only the keys THIS worktree owns and spare the sibling's;
// without the sibling-exclusion filter DropPrefix(bare) wipes the
// sibling's live data.
func TestDbResetSparesSiblingPrefixRedis(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitRedis(t)
	ctx := context.Background()

	// `:`-delimited prefix (idiomatic redis) exercises the delimiter-
	// agnostic sibling-token boundary — the bare prefix still nests the
	// sibling's `<prefix>:<slug>:*` keys.
	const activePrefix = "bsexactrd:"          // bare main-wt active prefix
	const siblingPrefix = "bsexactrd:feature:" // a linked wt's prefix — nests under the bare one
	mainKey := activePrefix + "a"
	siblingKey := siblingPrefix + "a"

	c := redisClient(t)
	_ = c.Del(ctx, mainKey, siblingKey).Err()
	t.Cleanup(func() { _ = c.Del(context.Background(), mainKey, siblingKey).Err() })

	rset(t, mainKey, "main")
	rset(t, siblingKey, "feature")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{URL: "redis://" + redisAddr},
		},
		// Bare key_prefix (no {slug}) models the main worktree's
		// overlay-resolved active prefix.
		Databases: []config.DatabaseConfig{{
			Engine:       "redis",
			KeyPrefix:    activePrefix,
			BranchScoped: true,
		}},
	}

	st := openStore(t)
	repoRoot := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		t.Fatal(err)
	}
	mainWtID, err := st.EnsureWorktree(ctx, repoID, repoRoot, "main", "develop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureWorktree(ctx, repoID, filepath.Join(repoRoot, "feature"), "feature", "feature"); err != nil {
		t.Fatal(err)
	}

	if err := prepare.ResetBranchScoped(ctx, cfg, repoRoot, repoID, mainWtID, st, ""); err != nil {
		t.Fatalf("ResetBranchScoped: %v", err)
	}

	if n, _ := c.Exists(ctx, mainKey).Result(); n != 0 {
		t.Fatalf("reset should have dropped the main worktree's own key %q", mainKey)
	}
	if n, _ := c.Exists(ctx, siblingKey).Result(); n != 1 {
		t.Fatalf("reset wiped sibling key %q via a bare-prefix match — sibling-isolation regression", siblingKey)
	}
}

// ─── TestParentSeedExcludesSiblingDataElasticsearch ──────────────────
//
// Parent-seed isolation for a prefix engine: when a feature branch seeds
// from a base branch that lives at the repo root, the base namespace is
// the bare main-worktree prefix — which is a prefix of every linked
// worktree's `<prefix>_<slug>_*` data. The seed must copy only the base
// worktree's OWN keys, never a sibling worktree's. Name-scoped engines get
// this free (exact-db copy); the prefix engines need the source filter.
// Without it the new branch is seeded with every sibling's data too.
func TestParentSeedExcludesSiblingDataElasticsearch(t *testing.T) {
	harness.SkipIfNoDocker(t)
	requireGit(t)
	waitES(t)

	ctx := context.Background()
	repoRoot := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "develop", repoRoot)
	mustGit(t, repoRoot, "config", "user.email", "t@t")
	mustGit(t, repoRoot, "config", "user.name", "t")
	writeFile(t, repoRoot, "README", "hi")
	mustGit(t, repoRoot, "add", "-A")
	mustGit(t, repoRoot, "commit", "-q", "-m", "init")
	mustGit(t, repoRoot, "remote", "add", "origin", repoRoot)
	mustGit(t, repoRoot, "update-ref", "refs/remotes/origin/develop", "refs/heads/develop")

	wtPath := filepath.Join(repoRoot, ".worktrees", "feature-x")
	mustGit(t, repoRoot, "worktree", "add", "-q", "-b", "feature/x", wtPath, "origin/develop")
	mustGit(t, wtPath, "branch", "--set-upstream-to=origin/develop", "feature/x")

	// main_worktree overlay makes the base (develop @ repo root) namespace a
	// bare, slug-free prefix `pp_`. Linked worktrees use `pp_{slug}_`.
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Elasticsearch: &config.EsConn{URL: esURL},
		},
		MainWorktree: config.MainWorktreeConfig{
			Enabled:   true,
			Databases: []config.DatabaseOverlay{{KeyPrefix: "pp_"}},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "elasticsearch",
			KeyPrefix:    "pp_{slug}_",
			BranchScoped: true,
		}},
	}

	// Base worktree's OWN data lives under the bare prefix: `pp_items`.
	esIndex(t, "pp_", "1", "base")
	// A sibling linked worktree's branch_scoped data nests under the bare
	// prefix: `pp_<siblingSlug>_items`. Register the sibling so the swap
	// layer knows its namespace, and plant junk it must NOT copy.
	siblingPath := filepath.Join(repoRoot, ".worktrees", "sibling")
	siblingSlug := slug.For(siblingPath, "").Value
	siblingPrefix := "pp_" + siblingSlug + "_"
	esIndex(t, siblingPrefix, "1", "sibling-junk")

	st := openStore(t)
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureWorktree(ctx, repoID, siblingPath, siblingSlug, "feature/sibling"); err != nil {
		t.Fatal(err)
	}
	featSlug := slug.For(wtPath, "")
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, featSlug.Value, "feature/x")
	if err != nil {
		t.Fatal(err)
	}

	featPrefix := "pp_" + featSlug.Value + "_"
	t.Cleanup(func() {
		dropESIndex(t, "pp_items")
		dropESIndex(t, siblingPrefix+"items")
		dropESIndex(t, featPrefix+"items")
		dropESIndex(t, featPrefix+siblingSlug+"_items") // the junk a broken seed would create
	})

	if _, err := prepare.Run(ctx, cfg, wtPath, featSlug, st, repoID, wtID, nil); err != nil {
		t.Fatalf("prepare feature/x: %v", err)
	}

	// Seeded from the base worktree's own data only.
	esAssertVals(t, featPrefix, map[string]string{"1": "base"})
	// The sibling's nested data must NOT have been dragged in.
	if n := esCount(t, featPrefix+siblingSlug+"_items*"); n != 0 {
		t.Fatalf("parent seed dragged in sibling data under %s%s_* (%d docs) — source-filter regression",
			featPrefix, siblingSlug, n)
	}
}

// ─── TestDbResetReseedsFromParentMySQL ───────────────────────────────
//
// The HIGH-severity regression, end-to-end against a real engine: after
// `db reset` the active DB must be re-seeded from the live parent branch,
// not left empty. Needs a real git repo so the feature branch has an
// upstream (origin/develop) that the base-branch resolver can follow back
// to develop, which lives at the repo root.
func TestDbResetReseedsFromParentMySQL(t *testing.T) {
	harness.SkipIfNoDocker(t)
	requireGit(t)
	waitMySQL(t)

	ctx := context.Background()
	repoRoot := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "develop", repoRoot)
	mustGit(t, repoRoot, "config", "user.email", "t@t")
	mustGit(t, repoRoot, "config", "user.name", "t")
	writeFile(t, repoRoot, "README", "hi")
	mustGit(t, repoRoot, "add", "-A")
	mustGit(t, repoRoot, "commit", "-q", "-m", "init")
	// Self-remote + a remote-tracking ref so the feature branch picks up
	// origin/develop as its upstream.
	mustGit(t, repoRoot, "remote", "add", "origin", repoRoot)
	mustGit(t, repoRoot, "update-ref", "refs/remotes/origin/develop", "refs/heads/develop")

	wtPath := filepath.Join(repoRoot, ".worktrees", "feature-x")
	mustGit(t, repoRoot, "worktree", "add", "-q", "-b", "feature/x", wtPath, "origin/develop")
	// Belt-and-braces: ensure the upstream is set even if autoSetupMerge
	// is disabled in the test environment's git config.
	mustGit(t, wtPath, "branch", "--set-upstream-to=origin/develop", "feature/x")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13390, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "tm_bsreset_{slug}",
			BranchScoped: true,
		}},
	}

	// The base lives at the repo root (HEAD=develop). Seed the exact DB
	// the base-branch resolver renders for (repoRoot, develop).
	parentDB := renderName(t, "tm_bsreset_{slug}", slug.ForMain(repoRoot, "develop"))
	dropMySQL(t, parentDB)
	mustExec(t, mysqlAdmin, "CREATE DATABASE "+parentDB)
	mustExec(t, parentDB, "CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32))")
	mustExec(t, parentDB, "INSERT INTO items(v) VALUES('base')")
	t.Cleanup(func() { dropMySQL(t, parentDB) })

	st := openStore(t)
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		t.Fatal(err)
	}
	sl := slug.For(wtPath, "")
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, "feature/x")
	if err != nil {
		t.Fatal(err)
	}

	// Initial prepare seeds the feature DB from the parent (develop).
	outs, err := prepare.Run(ctx, cfg, wtPath, sl, st, repoID, wtID, nil)
	if err != nil {
		t.Fatalf("prepare feature/x: %v", err)
	}
	active := outs[0].SourceDB
	t.Cleanup(func() { dropMySQL(t, active) })
	assertItems(t, active, "base") // seeded from parent

	// Diverge, then reset.
	mustExec(t, active, "INSERT INTO items(v) VALUES('local-change')")
	assertItems(t, active, "base", "local-change")

	if err := prepare.ResetBranchScoped(ctx, cfg, wtPath, repoID, wtID, st, ""); err != nil {
		t.Fatalf("ResetBranchScoped: %v", err)
	}
	if _, err := prepare.Run(ctx, cfg, wtPath, sl, st, repoID, wtID, nil); err != nil {
		t.Fatalf("prepare after reset: %v", err)
	}

	// Re-seeded from the live parent: the local divergence is gone and the
	// base row is back. (With the old Empty()-then-adopt bug this DB would
	// be empty here.)
	assertItems(t, active, "base")
}

// ─── TestDbResetDropsActiveExactNotPrefixMySQL ───────────────────────
//
// The data-loss regression: `db reset` must drop the EXACT active
// database, never a `LIKE 'name%'` prefix family. On the main worktree
// the branch_scoped active DB is the bare, overlay-resolved app name
// (e.g. `tm_bsexact`), which is a prefix of every linked worktree's
// `tm_bsexact_<slug>`. The pre-fix code routed the active drop through
// DropMatching (prefix), so a reset at the repo root wiped every sibling
// worktree's database. Here a reset on the bare-name active must leave
// the prefix-sharing sibling DB untouched.
func TestDbResetDropsActiveExactNotPrefixMySQL(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMySQL(t)
	ctx := context.Background()

	const mainDB = "tm_bsexact"            // bare active (main worktree, overlay-resolved)
	const siblingDB = "tm_bsexact_feature" // a linked worktree's active — shares the prefix
	for _, db := range []string{mainDB, siblingDB} {
		db := db
		dropMySQL(t, db)
		mustExec(t, mysqlAdmin, "CREATE DATABASE "+db)
		t.Cleanup(func() { dropMySQL(t, db) })
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13390, User: "root", Password: "rootpw"},
		},
		// No `{slug}` in the template → renders to the bare constant name,
		// modelling the main worktree's overlay-resolved active DB.
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: mainDB,
			BranchScoped: true,
		}},
	}

	st := openStore(t)
	repoRoot := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, repoRoot, "main", "develop")
	if err != nil {
		t.Fatal(err)
	}

	if err := prepare.ResetBranchScoped(ctx, cfg, repoRoot, repoID, wtID, st, ""); err != nil {
		t.Fatalf("ResetBranchScoped: %v", err)
	}

	if mysqlDBExists(t, mainDB) {
		t.Fatalf("reset should have dropped the exact active db %q", mainDB)
	}
	if !mysqlDBExists(t, siblingDB) {
		t.Fatalf("reset dropped sibling %q via a prefix match — exact-drop regression", siblingDB)
	}
}

// ─── TestWorktreeDeleteKeepsDurableMySQL ─────────────────────────────
//
// `wt delete` teardown must capture the active namespace into the current
// branch's durable copy and KEEP it, so re-creating the worktree (or
// re-preparing it) resumes the branch's divergent data rather than
// starting empty.
func TestWorktreeDeleteKeepsDurableMySQL(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMySQL(t)

	ctx := context.Background()
	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)
	sl := slug.For(wtPath, "")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13390, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "tm_bsdel_{slug}",
			BranchScoped: true,
		}},
	}

	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	t.Cleanup(func() { dropMySQL(t, active) })
	mustExec(t, active, "CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32))")
	mustExec(t, active, "INSERT INTO items(v) VALUES('feature-data')")
	assertItems(t, active, "feature-data")

	// Teardown (the `wt delete` DB path): captures durable, drops active.
	if err := prepare.TeardownDatabases(ctx, cfg, sl.Value, repoID, wtID, st); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	if exists := mysqlDBExists(t, active); exists {
		t.Fatalf("teardown should have dropped the active DB %s", active)
	}

	// Re-prepare on the same branch → resumes from the durable copy.
	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != active {
		t.Fatalf("active namespace drifted after re-create: %s → %s", active, got)
	}
	assertItems(t, active, "feature-data") // durable kept → resumed
}

// ─── TestReapBranchDurablesMySQL ─────────────────────────────────────
//
// Complement to TestWorktreeDeleteKeepsDurableMySQL: when a branch is
// DELETED (the daemon's auto-fetch prune of a merged, gone-upstream
// branch), ReapBranchDurables must drop that branch's durable copy — but
// ONLY that durable. The active namespace (live data the app connects to)
// and any other branch's durable must survive. A regression that dropped
// the active namespace would be silent live-data loss.
func TestReapBranchDurablesMySQL(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMySQL(t)

	ctx := context.Background()
	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13390, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "tm_bsreap_{slug}",
			BranchScoped: true,
		}},
	}

	// develop: seed schema + a row.
	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "develop")
	t.Cleanup(func() { dropMySQL(t, active) })
	mustExec(t, active, "CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32))")
	mustExec(t, active, "INSERT INTO items(v) VALUES('develop-data')")

	// Switch develop→feature so develop's data is captured into its durable
	// copy. The diff isolates the durable this switch created.
	before := listMySQLDBsWithPrefix(t, "_tmbs_")
	drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	mustExec(t, active, "INSERT INTO items(v) VALUES('feature-data')")
	newDurables := diffStrings(listMySQLDBsWithPrefix(t, "_tmbs_"), before)
	if len(newDurables) != 1 {
		t.Fatalf("expected exactly one durable from the develop→feature switch, got %v", newDurables)
	}
	developDurable := newDurables[0]
	t.Cleanup(func() { dropMySQL(t, developDurable) })

	// Reaping a branch with no durable (feature is live in active) is a
	// no-op: it must touch neither the active namespace nor develop's durable.
	prepare.ReapBranchDurables(ctx, cfg, st, repoID, "feature")
	if !mysqlDBExists(t, active) {
		t.Fatal("reaping a live branch must not drop the active namespace")
	}
	if !mysqlDBExists(t, developDurable) {
		t.Fatalf("reaping %q must not touch develop's durable %s", "feature", developDurable)
	}

	// Reaping the deleted branch drops ITS durable, and only that — the
	// active namespace and its live data survive.
	prepare.ReapBranchDurables(ctx, cfg, st, repoID, "develop")
	if mysqlDBExists(t, developDurable) {
		t.Fatalf("ReapBranchDurables(develop) must drop develop's durable %s", developDurable)
	}
	if !mysqlDBExists(t, active) {
		t.Fatal("reap must never drop the active namespace (live data)")
	}
	assertItems(t, active, "develop-data", "feature-data")
}

// ─── TestWorktreeDeleteKeepsDurablePostgres ──────────────────────────
//
// Postgres mirror of TestWorktreeDeleteKeepsDurableMySQL: `wt delete`
// teardown captures the active DB into the current branch's durable copy
// and drops the active, so re-creating the worktree on the same branch
// resumes the diverged data.
func TestWorktreeDeleteKeepsDurablePostgres(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitPostgres(t)

	ctx := context.Background()
	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)
	sl := slug.For(wtPath, "")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Postgres: &config.PostgresConn{Host: "127.0.0.1", Port: 15490, User: "postgres", Password: "pgpw"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "postgres",
			NameTemplate: "tm_bsdelpg_{slug}",
			BranchScoped: true,
		}},
	}

	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	pgExec(t, active, "CREATE TABLE items (id SERIAL PRIMARY KEY, v TEXT)")
	pgExec(t, active, "INSERT INTO items(v) VALUES('feature-data')")
	pgAssertItems(t, active, "feature-data")

	// Teardown (the `wt delete` DB path): captures durable, drops active.
	if err := prepare.TeardownDatabases(ctx, cfg, sl.Value, repoID, wtID, st); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	if pgDBExists(t, active) {
		t.Fatalf("teardown should have dropped the active DB %s", active)
	}

	// Re-prepare on the same branch → resumes from the durable copy.
	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != active {
		t.Fatalf("active namespace drifted after re-create: %s → %s", active, got)
	}
	pgAssertItems(t, active, "feature-data") // durable kept → resumed
}

// ─── TestWorktreeDeleteKeepsDurableMongo ─────────────────────────────
//
// MongoDB mirror of the MySQL/Postgres delete case.
func TestWorktreeDeleteKeepsDurableMongo(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitMongo(t)

	ctx := context.Background()
	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)
	sl := slug.For(wtPath, "")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mongodb: &config.MongoConn{URI: mongoURI},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mongodb",
			NameTemplate: "tm_bsdelmongo_{slug}",
			BranchScoped: true,
		}},
	}

	active := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	mongoInsert(t, active, "feature-data")
	mongoAssert(t, active, "feature-data")

	if err := prepare.TeardownDatabases(ctx, cfg, sl.Value, repoID, wtID, st); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	if mongoDBExists(t, active) {
		t.Fatalf("teardown should have dropped the active DB %s", active)
	}

	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != active {
		t.Fatalf("active namespace drifted after re-create: %s → %s", active, got)
	}
	mongoAssert(t, active, "feature-data") // durable kept → resumed
}

// ─── TestWorktreeDeleteKeepsDurableRedis ─────────────────────────────
//
// Redis mirror, prefix-scoped: teardown drops every key under the active
// prefix but keeps the per-branch durable copy.
func TestWorktreeDeleteKeepsDurableRedis(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitRedis(t)

	ctx := context.Background()
	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)
	sl := slug.For(wtPath, "")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{URL: "redis://" + redisAddr},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "redis",
			KeyPrefix:    "bsdel:{slug}:",
			BranchScoped: true,
		}},
	}

	prefix := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	rset(t, prefix+"a", "feature-data")
	assertRedisVals(t, prefix, map[string]string{"a": "feature-data"})

	if err := prepare.TeardownDatabases(ctx, cfg, sl.Value, repoID, wtID, st); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	assertRedisVals(t, prefix, map[string]string{}) // active keys dropped

	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != prefix {
		t.Fatalf("active prefix drifted after re-create: %s → %s", prefix, got)
	}
	assertRedisVals(t, prefix, map[string]string{"a": "feature-data"}) // durable kept → resumed
}

// ─── TestWorktreeDeleteKeepsDurableElasticsearch ─────────────────────
//
// Elasticsearch mirror, prefix-scoped: teardown drops every index under
// the active prefix but keeps the per-branch durable copy.
func TestWorktreeDeleteKeepsDurableElasticsearch(t *testing.T) {
	harness.SkipIfNoDocker(t)
	waitES(t)

	ctx := context.Background()
	wtPath := t.TempDir()
	st := openStore(t)
	repoID, wtID := registerWorktree(t, st, wtPath)
	sl := slug.For(wtPath, "")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Elasticsearch: &config.EsConn{URL: esURL},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "elasticsearch",
			KeyPrefix:    "bsdeles_{slug}_",
			BranchScoped: true,
		}},
	}

	prefix := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature")
	esIndex(t, prefix, "a", "feature-data")
	esAssertVals(t, prefix, map[string]string{"a": "feature-data"})

	if err := prepare.TeardownDatabases(ctx, cfg, sl.Value, repoID, wtID, st); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	if n := esCount(t, prefix+"*"); n != 0 {
		t.Fatalf("teardown should have dropped active indices under %s*, found %d docs", prefix, n)
	}

	if got := drivePrepare(t, st, cfg, wtPath, repoID, wtID, "feature"); got != prefix {
		t.Fatalf("active prefix drifted after re-create: %s → %s", prefix, got)
	}
	esAssertVals(t, prefix, map[string]string{"a": "feature-data"}) // durable kept → resumed
}

// ─── helpers: shared ─────────────────────────────────────────────────

func renderName(t *testing.T, tmpl string, sl slug.Slug) string {
	t.Helper()
	out, err := template.Render(tmpl, template.FromSlug(sl))
	if err != nil {
		t.Fatalf("render %q: %v", tmpl, err)
	}
	return out
}

// mysqlAdmin is the connection target for server-level statements
// (CREATE DATABASE) — an empty DB name in the DSN.
const mysqlAdmin = ""

func mysqlDBExists(t *testing.T, dbName string) bool {
	t.Helper()
	db := mysqlDB(t, "")
	defer db.Close()
	var n int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", dbName).Scan(&n); err != nil {
		t.Fatalf("schema check %s: %v", dbName, err)
	}
	return n == 1
}

// ─── helpers: postgres ───────────────────────────────────────────────

func pgConn(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("postgres://postgres:pgpw@%s/%s?sslmode=disable", pgAddr, dbName)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg %s: %v", dbName, err)
	}
	return db
}

func pgExec(t *testing.T, dbName, q string) {
	t.Helper()
	db := pgConn(t, dbName)
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("pg exec on %s: %q: %v", dbName, q, err)
	}
}

func pgAssertItems(t *testing.T, dbName string, want ...string) {
	t.Helper()
	db := pgConn(t, dbName)
	defer db.Close()
	rows, err := db.Query("SELECT v FROM items ORDER BY v")
	if err != nil {
		t.Fatalf("pg read items from %s: %v", dbName, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pg %s items = %v, want %v", dbName, got, want)
	}
}

// pgDBExists reports whether a database named `name` lives in the
// cluster — checked from the `postgres` maintenance DB so it works even
// after `name` has been dropped.
func pgDBExists(t *testing.T, name string) bool {
	t.Helper()
	db := pgConn(t, "postgres")
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n); err != nil {
		t.Fatalf("pg exists %s: %v", name, err)
	}
	return n == 1
}

func waitPostgres(t *testing.T) {
	harness.WaitForReady(t, "postgres:"+pgAddr, 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", pgAddr, time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		db := pgConn(t, "postgres")
		defer db.Close()
		return db.Ping()
	})
}

// ─── helpers: mongo ──────────────────────────────────────────────────

func mongoConn(t *testing.T) *mongo.Client {
	t.Helper()
	c, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	return c
}

func mongoInsert(t *testing.T, dbName, v string) {
	t.Helper()
	c := mongoConn(t)
	if _, err := c.Database(dbName).Collection("items").InsertOne(context.Background(), bson.M{"v": v}); err != nil {
		t.Fatalf("mongo insert %s: %v", v, err)
	}
}

func mongoAssert(t *testing.T, dbName string, want ...string) {
	t.Helper()
	c := mongoConn(t)
	ctx := context.Background()
	cur, err := c.Database(dbName).Collection("items").Find(ctx, bson.D{})
	if err != nil {
		t.Fatalf("mongo find %s: %v", dbName, err)
	}
	var docs []struct {
		V string `bson:"v"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		t.Fatalf("mongo decode %s: %v", dbName, err)
	}
	var got []string
	for _, d := range docs {
		got = append(got, d.V)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mongo %s items = %v, want %v", dbName, got, want)
	}
}

// mongoDBExists reports whether a database named `name` is listed by the
// server. Mongo creates databases lazily, so a dropped name is absent.
func mongoDBExists(t *testing.T, name string) bool {
	t.Helper()
	c := mongoConn(t)
	names, err := c.ListDatabaseNames(context.Background(), bson.M{"name": name})
	if err != nil {
		t.Fatalf("mongo list dbs: %v", err)
	}
	return len(names) == 1
}

func waitMongo(t *testing.T) {
	harness.WaitForReady(t, "mongo:"+mongoURI, 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:27190", time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		cl, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
		if err != nil {
			return err
		}
		defer cl.Disconnect(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return cl.Ping(ctx, nil)
	})
}

// ─── helpers: elasticsearch ──────────────────────────────────────────

func esIndex(t *testing.T, prefix, id, v string) {
	t.Helper()
	index := prefix + "items"
	body, _ := json.Marshal(map[string]string{"v": v})
	url := fmt.Sprintf("%s/%s/_doc/%s?refresh=true", esURL, index, id)
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("es index %s/%s: %v", index, id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("es index %s/%s: %s: %s", index, id, resp.Status, b)
	}
}

// dropESIndex deletes one index by EXACT name. 200 (deleted) and 404
// (already absent) are both fine — used for test pre-clean + cleanup.
// Exact name avoids the wildcard-DELETE rejection ES enforces when
// action.destructive_requires_name is on (the cluster default).
func dropESIndex(t *testing.T, index string) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", esURL+"/"+index, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("es delete %s: %v", index, err)
	}
	resp.Body.Close()
}

func esAssertVals(t *testing.T, prefix string, want map[string]string) {
	t.Helper()
	index := prefix + "items"
	// Refresh so a freshly-restored index is searchable.
	_, _ = http.Post(esURL+"/"+index+"/_refresh", "application/json", nil)
	req, _ := http.NewRequest("GET", esURL+"/"+index+"/_search?size=100", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("es search %s: %v", index, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("es search %s: %s: %s", index, resp.Status, b)
	}
	var out struct {
		Hits struct {
			Hits []struct {
				ID     string            `json:"_id"`
				Source map[string]string `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("es parse %s: %v: %s", index, err, b)
	}
	got := map[string]string{}
	for _, h := range out.Hits.Hits {
		got[h.ID] = h.Source["v"]
	}
	if len(got) != len(want) {
		t.Fatalf("es %s: got %v, want %v", index, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("es %s key %q = %q, want %q (full: %v)", index, k, got[k], v, got)
		}
	}
}

// esCount returns the document count across every index matching the
// wildcard `pattern` (e.g. "bsdeles_<slug>_*"). A pattern that matches no
// index returns 0 (wildcard expansion to empty is allowed), which is the
// clean "namespace is gone" signal after a teardown.
func esCount(t *testing.T, pattern string) int {
	t.Helper()
	resp, err := http.Get(esURL + "/" + pattern + "/_count")
	if err != nil {
		t.Fatalf("es count %s: %v", pattern, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("es count %s: %s: %s", pattern, resp.Status, body)
	}
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("es count parse %s: %v: %s", pattern, err, body)
	}
	return out.Count
}

func waitES(t *testing.T) {
	harness.WaitForReady(t, "es:"+esURL, 120*time.Second, func() error {
		resp, err := http.Get(esURL + "/_cluster/health?wait_for_status=yellow&timeout=5s")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("es health %s", resp.Status)
		}
		return nil
	})
}
