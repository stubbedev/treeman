//go:build e2e

// Package coldbuild_siblings_e2e proves that running prepare in the
// main worktree (with a main_worktree overlay that rebinds source
// names to bare values) does NOT wipe a sibling linked worktree's
// `<source>_<slug>` databases / per-test clones. This is the
// production incident regression test: pre-fix, the cold-build
// pre-drop used DropMatching(sourceDB) which is a prefix LIKE, so
// main-wt's `sibling_e2e` source-DB drop also nuked
// `sibling_e2e_<slug>`, `sibling_e2e_<slug>_test_1..N`, and
// `sibling_e2e_<slug>` (mongo) for every linked worktree.
//
// Runs against real mysql + mongo containers so the fix is verified
// at the same level the user observed the bug (a `SHOW DATABASES`
// after a main-wt prepare). Excluded from default `go test ./...`
// via the `e2e` build tag; runs under `just test-e2e`.
package coldbuild_siblings_e2e

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
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

const (
	mysqlAddr = "127.0.0.1:13326"
	mongoURI  = "mongodb://127.0.0.1:27127"
	redisURL  = "redis://127.0.0.1:16399"
	esURL     = "http://127.0.0.1:19299"
)

func TestSiblingWorktreeDBsSurviveMainWtColdBuild(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "mysql:"+mysqlAddr, 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", mysqlAddr, 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
	harness.WaitForReady(t, "mongo:27127", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:27127", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
	harness.WaitForReady(t, "redis:16399", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:16399", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
	harness.WaitForReady(t, "es:19299", 120*time.Second, func() error {
		resp, err := http.Get(esURL + "/_cluster/health")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	})

	ctx := context.Background()

	// One store shared between both worktrees so the cold-build
	// siblingSlugs() lookup actually returns the sibling's slug.
	dbPath := filepath.Join(t.TempDir(), "treeman-test.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	siblingPath := t.TempDir()
	mainPath := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, mainPath, "siblings-e2e")
	if err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	siblingSlug := slug.Slug{Value: "kon_12463", Source: slug.SourceTicket}
	mainSlug := slug.Slug{Value: "main_master", Source: slug.SourceTicket}
	siblingWtID, err := st.EnsureWorktree(ctx, repoID, siblingPath, siblingSlug.Value, "bugfix/KON-12463")
	if err != nil {
		t.Fatalf("ensure sibling wt: %v", err)
	}
	mainWtID, err := st.EnsureWorktree(ctx, repoID, mainPath, mainSlug.Value, "master")
	if err != nil {
		t.Fatalf("ensure main wt: %v", err)
	}

	// Run prepare for the linked (sibling) worktree first. Mysql
	// uses fanout into per-test clones; mongo without a seed step
	// is a no-op, so we pre-seed the mongo sibling DB directly via
	// the client below.
	siblingCfg := siblingConfig()
	siblingOuts, err := prepare.Run(ctx, siblingCfg, siblingPath, siblingSlug, st, repoID, siblingWtID, nil)
	if err != nil {
		t.Fatalf("sibling prepare: %v", err)
	}
	siblingMysql := outcomeFor(t, siblingOuts, "mysql")
	t.Logf("sibling mysql sourceDB=%s clones=%v", siblingMysql.SourceDB, siblingMysql.Clones)

	// Pre-populate sibling state for engines that don't materialise
	// without writes (mongo) or whose source data needs a real key
	// to survive a prefix scan (redis / es).
	mustInsertMongoMarker(t, "sibling_mongo_"+siblingSlug.Value)
	mustSetRedisKeys(t, []string{
		"sibling_redis_" + siblingSlug.Value + "_data:1",
		"sibling_redis_" + siblingSlug.Value + "_data:2",
	})
	mustCreateESIndex(t, "sibling_kho_"+siblingSlug.Value+"_data")

	sibMysqlBefore := listMysqlDatabasesWithPrefix(t, "sibling_e2e_")
	sibMongoBefore := listMongoDatabasesWithPrefix(t, "sibling_mongo_")
	sibRedisBefore := listRedisKeysWithPrefix(t, "sibling_redis_"+siblingSlug.Value)
	sibESBefore := listESIndicesWithPrefix(t, "sibling_kho_"+siblingSlug.Value)
	if len(sibMysqlBefore) < 1+1 { // source + at least 1 paratest clone
		t.Fatalf("sibling prepare didn't create the expected mysql DBs: %v", sibMysqlBefore)
	}
	if len(sibMongoBefore) < 1 {
		t.Fatalf("sibling mongo DB pre-populate didn't materialise: %v", sibMongoBefore)
	}
	if len(sibRedisBefore) < 2 {
		t.Fatalf("sibling redis keys didn't materialise: %v", sibRedisBefore)
	}
	if len(sibESBefore) < 1 {
		t.Fatalf("sibling es index didn't materialise: %v", sibESBefore)
	}

	// Post-v5 the snapshot fingerprint is content-only: the main
	// worktree (bare `sibling_e2e`) and the sibling share inputs, so
	// they share a fingerprint. The main prepare would otherwise CACHE
	// HIT off the sibling's just-built template and never run the
	// cold-build DROP this regression test exists to guard. Clear the
	// repo's snapshot rows to force a cache miss so the bare-named cold
	// build (and its exact DropDatabase / slug-anchored prefix drops)
	// is actually exercised.
	cands, err := st.ListSnapshotsForRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	for _, c := range cands {
		if err := st.DeleteSnapshot(ctx, c.Fingerprint); err != nil {
			t.Fatalf("clear snapshot %s: %v", c.Fingerprint, err)
		}
	}

	// Now run prepare for the MAIN worktree with the main_worktree
	// overlay applied. Overlay rebinds the source name to a bare
	// `sibling_e2e` / `sibling_mongo` — under the pre-fix code this
	// is the call that wiped every sibling DB sharing the prefix.
	mainCfg := mainConfig()
	if _, err := prepare.Run(ctx, mainCfg, mainPath, mainSlug, st, repoID, mainWtID, nil); err != nil {
		t.Fatalf("main prepare: %v", err)
	}

	// Assert main wt's bare-named mysql source DB exists. (Mongo
	// without a seed step has no data so Mongo's lazy DB
	// materialisation never creates `sibling_mongo` — the
	// observability bug for that engine is the survival of
	// `sibling_mongo_<slug>`, which the next assertion covers.)
	mainMysqlDBs := listMysqlDatabasesWithPrefix(t, "sibling_e2e")
	if !contains(mainMysqlDBs, "sibling_e2e") {
		t.Errorf("main mysql source DB `sibling_e2e` missing: got %v", mainMysqlDBs)
	}

	// Every sibling DB from before MUST still exist — this is the
	// production-incident regression test.
	sibMysqlAfter := listMysqlDatabasesWithPrefix(t, "sibling_e2e_")
	missingMysql := setDiff(sibMysqlBefore, sibMysqlAfter)
	if len(missingMysql) > 0 {
		t.Errorf("MYSQL SIBLING-WIPE REGRESSION: %d sibling DBs disappeared after main-wt prepare: %v\nbefore=%v\nafter=%v",
			len(missingMysql), missingMysql, sibMysqlBefore, sibMysqlAfter)
	}
	sibMongoAfter := listMongoDatabasesWithPrefix(t, "sibling_mongo_")
	missingMongo := setDiff(sibMongoBefore, sibMongoAfter)
	if len(missingMongo) > 0 {
		t.Errorf("MONGO SIBLING-WIPE REGRESSION: %d sibling DBs disappeared after main-wt prepare: %v\nbefore=%v\nafter=%v",
			len(missingMongo), missingMongo, sibMongoBefore, sibMongoAfter)
	}
	sibRedisAfter := listRedisKeysWithPrefix(t, "sibling_redis_"+siblingSlug.Value)
	missingRedis := setDiff(sibRedisBefore, sibRedisAfter)
	if len(missingRedis) > 0 {
		t.Errorf("REDIS SIBLING-WIPE REGRESSION: %d sibling keys disappeared after main-wt prepare: %v\nbefore=%v\nafter=%v",
			len(missingRedis), missingRedis, sibRedisBefore, sibRedisAfter)
	}
	sibESAfter := listESIndicesWithPrefix(t, "sibling_kho_"+siblingSlug.Value)
	missingES := setDiff(sibESBefore, sibESAfter)
	if len(missingES) > 0 {
		t.Errorf("ES SIBLING-WIPE REGRESSION: %d sibling indices disappeared after main-wt prepare: %v\nbefore=%v\nafter=%v",
			len(missingES), missingES, sibESBefore, sibESAfter)
	}
}

func siblingConfig() *config.Config {
	clones := config.ClonesSetting{Fixed: 2}
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql:         &config.MysqlConn{Host: "127.0.0.1", Port: 13326, User: "root", Password: "rootpw"},
			Mongodb:       &config.MongoConn{URI: mongoURI},
			Redis:         &config.RedisConn{URL: redisURL},
			Elasticsearch: &config.EsConn{URL: esURL},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "sibling_e2e_{slug}",
				TestClones: &config.TestClonesSpec{
					Clones:       clones,
					NameTemplate: "sibling_e2e_{slug}_test_{n}",
				},
			},
			{
				// No Seed: mongo cold-build is still exercised
				// (DropDatabase + snapshot), but no per-prepare data
				// write. The sibling mongo DB is materialised
				// directly by the test via mustInsertMongoMarker.
				Engine:       "mongodb",
				NameTemplate: "sibling_mongo_{slug}",
			},
			{
				// Redis is prefix-scoped: cold-build's pre-drop
				// previously used the bare DropPrefix which would
				// reap every sibling worktree's `<prefix>_<slug>_*`
				// keys under main-wt overlay. Now uses
				// DropPrefixFiltered with the slug-anchored keep
				// predicate.
				Engine:    "redis",
				KeyPrefix: "sibling_redis_{slug}_",
			},
			{
				// Elasticsearch indices: cold-build previously used
				// DropMatching on the bare prefix, nuking every
				// sibling's `<prefix>_<slug>_*` indices. Now uses
				// DropMatchingFiltered with the same slug-anchored
				// predicate.
				Engine:    "elasticsearch",
				KeyPrefix: "sibling_kho_{slug}_",
			},
		},
	}
}

func mainConfig() *config.Config {
	// Same base; overlay rebinds all four to bare names (no {slug}).
	cfg := siblingConfig()
	cfg.MainWorktree = config.MainWorktreeConfig{
		Enabled: true,
		Databases: []config.DatabaseOverlay{
			{
				NameTemplate: "sibling_e2e",
				TestClones: &config.TestClonesSpec{
					Clones:       config.ClonesSetting{Fixed: 2},
					NameTemplate: "sibling_e2e_test_{n}",
				},
			},
			{NameTemplate: "sibling_mongo"},
			{KeyPrefix: "sibling_redis_"},
			{KeyPrefix: "sibling_kho_"},
		},
	}
	// prepare.Run respects the overlay only when the caller has
	// applied it; finalize.go does this for main-wt finalizes. We
	// invoke prepare.Run directly so apply explicitly.
	config.ApplyMainWorktreeOverlay(cfg)
	return cfg
}

func outcomeFor(t *testing.T, outs []prepare.Outcome, engine string) prepare.Outcome {
	t.Helper()
	for _, o := range outs {
		if o.Engine == engine {
			return o
		}
	}
	t.Fatalf("no outcome for engine %q", engine)
	return prepare.Outcome{}
}

func listMysqlDatabasesWithPrefix(t *testing.T, prefix string) []string {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("root:rootpw@tcp(%s)/", mysqlAddr))
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(),
		"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ?",
		prefix+"%")
	if err != nil {
		t.Fatalf("list mysql dbs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func mustInsertMongoMarker(t *testing.T, dbName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer c.Disconnect(ctx)
	_, err = c.Database(dbName).Collection("marker").InsertOne(ctx, map[string]any{"k": 1})
	if err != nil {
		t.Fatalf("seed mongo %s: %v", dbName, err)
	}
}

func listMongoDatabasesWithPrefix(t *testing.T, prefix string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer c.Disconnect(ctx)
	names, err := c.ListDatabaseNames(ctx, struct{}{})
	if err != nil {
		t.Fatalf("list mongo dbs: %v", err)
	}
	var out []string
	for _, n := range names {
		if len(n) >= len(prefix) && n[:len(prefix)] == prefix {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func mustSetRedisKeys(t *testing.T, keys []string) {
	t.Helper()
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("redis parse: %v", err)
	}
	c := redis.NewClient(opt)
	defer c.Close()
	for _, k := range keys {
		if err := c.Set(context.Background(), k, "1", 0).Err(); err != nil {
			t.Fatalf("redis SET %s: %v", k, err)
		}
	}
}

func listRedisKeysWithPrefix(t *testing.T, prefix string) []string {
	t.Helper()
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("redis parse: %v", err)
	}
	c := redis.NewClient(opt)
	defer c.Close()
	iter := c.Scan(context.Background(), 0, prefix+"*", 100).Iterator()
	var out []string
	for iter.Next(context.Background()) {
		out = append(out, iter.Val())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("redis SCAN: %v", err)
	}
	sort.Strings(out)
	return out
}

func mustCreateESIndex(t *testing.T, name string) {
	t.Helper()
	req, _ := http.NewRequest("PUT", esURL+"/"+name, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("es create %s: %v", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Fatalf("es create %s: HTTP %d: %s", name, resp.StatusCode, string(body))
	}
}

func listESIndicesWithPrefix(t *testing.T, prefix string) []string {
	t.Helper()
	resp, err := http.Get(esURL + "/_cat/indices/" + prefix + "*?h=index&format=json")
	if err != nil {
		t.Fatalf("es list: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Fatalf("es list: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var rows []struct {
		Index string `json:"index"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		// Fallback: text format
		var out []string
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		sort.Strings(out)
		return out
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Index)
	}
	sort.Strings(out)
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func setDiff(want, got []string) []string {
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	var missing []string
	for _, w := range want {
		if !gotSet[w] {
			missing = append(missing, w)
		}
	}
	return missing
}

// TestESDumpRoundTrip exercises the new dbes.Driver.Dump path
// (wired into MCP db_dump for engine=elasticsearch). Creates indices
// + docs under one prefix, dumps to a temp NDJSON file, drops the
// originals, restores into a different prefix, asserts every doc is
// present under the new prefix. Catches regressions in scroll
// handling, `{target_db}` substitution, and the round-trip with the
// existing dbes.Driver.Restore.
func TestESDumpRoundTrip(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "es:19299", 120*time.Second, func() error {
		resp, err := http.Get(esURL + "/_cluster/health")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	})

	ctx := context.Background()
	srcPrefix := "rtdump_src_"
	dstPrefix := "rtdump_dst_"

	// Clean slate.
	mustDeleteESIndices(t, srcPrefix+"*")
	mustDeleteESIndices(t, dstPrefix+"*")

	mustCreateESIndex(t, srcPrefix+"users")
	mustCreateESIndex(t, srcPrefix+"orders")
	mustIndexESDoc(t, srcPrefix+"users", "u1", `{"name":"alice"}`)
	mustIndexESDoc(t, srcPrefix+"users", "u2", `{"name":"bob"}`)
	mustIndexESDoc(t, srcPrefix+"orders", "o1", `{"total":100}`)
	mustRefreshESIndex(t, srcPrefix+"*")

	drv, err := dbes.Connect(ctx, config.EsConn{URL: esURL})
	if err != nil {
		t.Fatalf("es connect: %v", err)
	}

	dumpPath := filepath.Join(t.TempDir(), "rtdump.ndjson")
	f, err := openWritable(dumpPath)
	if err != nil {
		t.Fatalf("create dump: %v", err)
	}
	if err := drv.Dump(ctx, srcPrefix, f); err != nil {
		t.Fatalf("dump: %v", err)
	}
	_ = f.Close()

	// Drop the originals before restoring so the test failure mode
	// is "docs missing" not "docs duplicated".
	mustDeleteESIndices(t, srcPrefix+"*")

	if err := drv.Restore(ctx, dstPrefix, dumpPath); err != nil {
		t.Fatalf("restore: %v", err)
	}
	mustRefreshESIndex(t, dstPrefix+"*")

	if got := esDocCount(t, dstPrefix+"users"); got != 2 {
		t.Errorf("dst users count = %d, want 2", got)
	}
	if got := esDocCount(t, dstPrefix+"orders"); got != 1 {
		t.Errorf("dst orders count = %d, want 1", got)
	}
	// Confirm the {target_db} substitution actually rewrote the
	// _index — restored docs must live under dstPrefix, not under
	// the literal `{target_db}users`.
	if got := esIndexExists(t, "{target_db}users"); got {
		t.Errorf("literal `{target_db}users` index exists — substitution didn't fire")
	}

	mustDeleteESIndices(t, dstPrefix+"*")
}

func mustIndexESDoc(t *testing.T, index, id, body string) {
	t.Helper()
	req, _ := http.NewRequest("PUT", esURL+"/"+index+"/_doc/"+id, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("index doc: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("index doc %s/%s: HTTP %d: %s", index, id, resp.StatusCode, string(body))
	}
}

func mustRefreshESIndex(t *testing.T, pattern string) {
	t.Helper()
	resp, err := http.Post(esURL+"/"+pattern+"/_refresh", "application/json", nil)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	_ = resp.Body.Close()
}

func mustDeleteESIndices(t *testing.T, pattern string) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", esURL+"/"+pattern+"?ignore_unavailable=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = resp.Body.Close()
}

func esDocCount(t *testing.T, index string) int {
	t.Helper()
	resp, err := http.Get(esURL + "/" + index + "/_count")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("count parse: %v (body=%s)", err, string(body))
	}
	return r.Count
}

func esIndexExists(t *testing.T, index string) bool {
	t.Helper()
	resp, err := http.Head(esURL + "/" + index)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == 200
}

func openWritable(path string) (io.WriteCloser, error) {
	return os.Create(path)
}
