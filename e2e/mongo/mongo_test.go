//go:build e2e

package mongo_e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
)

func TestMongoEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	// A real connect+ping, not just a TCP dial: a fresh mongod accepts
	// connections a beat before it durably serves writes, and the seed
	// races that window under load. Gate on an actual ping so prepare
	// (and its seed) only runs once mongod is truly serving.
	harness.WaitForReady(t, "mongo:27117", 60*time.Second, func() error {
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:27117"))
		if err != nil {
			return err
		}
		defer c.Disconnect(pingCtx)
		return c.Ping(pingCtx, nil)
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	cfg := buildConfig()
	env := harness.NewEnv(t, wt)

	// Pass 1: cold build (seed populates the source DB, snapshot
	// caches it per-collection, fanout into clones).
	outs := env.RunPrepare(t, cfg)
	o1 := harness.AssertOutcome(t, outs, "mongodb", false)
	t.Logf("pass1: sourceDB=%s template=%s", o1.SourceDB, o1.TemplateName)
	assertProductCount(t, o1.SourceDB, 3)

	// Pass 2: cache hit.
	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "mongodb", true)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("fingerprint drift: %s vs %s", o1.Fingerprint, o2.Fingerprint)
	}

	// Pass 3: edit an input file → cold rebuild.
	if err := os.WriteFile(filepath.Join(wt, "fixtures/schema.txt"),
		[]byte("products schema v2\norders schema v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outs = env.RunPrepare(t, cfg)
	o3 := harness.AssertOutcome(t, outs, "mongodb", false)
	if o3.Fingerprint == o1.Fingerprint {
		t.Errorf("fingerprint unchanged after schema.txt edit")
	}

	// Fanout setup permutation: the 2 declared clones exist with the
	// seeded products (restored from the template).
	if len(o3.Clones) != 2 {
		t.Fatalf("fanout: got %d clones, want 2 (%v)", len(o3.Clones), o3.Clones)
	}
	for _, c := range o3.Clones {
		if !mongoDBExists(t, c) {
			t.Errorf("clone DB %s missing after fanout", c)
		}
		assertProductCount(t, c, 3)
	}

	// ── teardown: `wt delete` drops the per-worktree DBs, keeps the cache ──
	// TeardownDatabases is the DB layer of `treeman wt delete`: it must DROP
	// the source database AND every clone while leaving the fingerprint-keyed
	// template intact, so the next prepare with the same inputs is a cache hit.
	if err := prepare.TeardownDatabases(env.Ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	if mongoDBExists(t, o3.SourceDB) {
		t.Errorf("source DB %s still exists after teardown", o3.SourceDB)
	}
	for _, c := range o3.Clones {
		if mongoDBExists(t, c) {
			t.Errorf("clone DB %s still exists after teardown", c)
		}
	}
	// The fingerprint-keyed template must SURVIVE teardown so the next
	// worktree with the same inputs still hits the cache.
	if !mongoDBExists(t, o3.TemplateName) {
		t.Errorf("template DB %s was dropped by teardown (cache must survive)", o3.TemplateName)
	}
}

// mongoDBExists reports whether a database named `name` is listed by the
// server. Mongo creates databases lazily, so a dropped name is absent.
func mongoDBExists(t *testing.T, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:27117"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Disconnect(ctx)
	names, err := c.ListDatabaseNames(ctx, bson.M{"name": name})
	if err != nil {
		t.Fatalf("mongo list dbs: %v", err)
	}
	return len(names) == 1
}

func buildConfig() *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Mongodb: &config.MongoConn{URI: "mongodb://127.0.0.1:27117"},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mongodb",
				NameTemplate: "treeman_e2e_{slug}",
				Seed: &config.Step{
					Run: "./fixtures/seed.sh",
					Env: map[string]string{
						"MONGO_DB":        "{target_db}",
						"MONGO_CONTAINER": "treeman-e2e-mongo",
					},
				},
				Inputs: []config.Input{
					{Glob: "fixtures/schema.txt", Label: "schema"},
				},
				// Fanout: pre-warm 2 clones so teardown's per-worktree
				// cleanup (source + clones) is exercised.
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

func assertProductCount(t *testing.T, dbName string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:27117"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Disconnect(ctx)
	n, err := c.Database(dbName).Collection("products").CountDocuments(ctx, struct{}{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if int(n) != want {
		t.Errorf("products count = %d, want %d (db=%s)", n, want, dbName)
	}
}
