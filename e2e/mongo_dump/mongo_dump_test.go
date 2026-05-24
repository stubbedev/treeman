//go:build e2e

// Package mongo_dump_e2e exercises treeman's real mongorestore path
// (vs the seed-script path the mongo/ suite uses). Sequence:
//
//   1. boot mongo container
//   2. seed source data via mongosh
//   3. mongodump --archive into a fixture file
//   4. drop the seeded data
//   5. point treeman's prepare at the archive
//   6. confirm the rehydrated DB has the expected docs
//
// Tests both uncompressed AND gzip-compressed archives.
package mongo_dump_e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

const (
	mongoURI       = "mongodb://127.0.0.1:27127"
	mongoContainer = "treeman-e2e-mongodump"
	seedDB         = "seed_source"
)

func TestMongoArchiveRestore(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("mongorestore"); err != nil {
		t.Skip("mongorestore not on PATH")
	}
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mongo:27127", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:27127", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	// Seed source DB with two collections.
	seed(t)
	// Generate two archive fixtures: plain + gzip.
	archivePath := filepath.Join(t.TempDir(), "seed.archive")
	gzipPath := filepath.Join(t.TempDir(), "seed.archive.gz")
	mongodump(t, archivePath, false)
	mongodump(t, gzipPath, true)
	// Drop source so the restore is observable.
	dropSeedDB(t)

	cases := []struct {
		name    string
		archive string
	}{
		{"uncompressed", archivePath},
		{"gzip", gzipPath},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wt := t.TempDir()
			fixturePath := filepath.Join(wt, "dump.archive")
			body, err := os.ReadFile(c.archive)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixturePath, body, 0o644); err != nil {
				t.Fatal(err)
			}

			cfg := &config.Config{
				Connections: config.ConnectionsConfig{
					Mongodb: &config.MongoConn{URI: mongoURI},
				},
				Databases: []config.DatabaseConfig{
					{
						Engine:       "mongodb",
						NameTemplate: fmt.Sprintf("tm_mgd_%s_{slug}", c.name),
						Dump: &config.DumpSpec{
							Path:     "dump.archive",
							SourceDB: seedDB,
						},
					},
				},
			}
			env := harness.NewEnv(t, wt)
			outs := env.RunPrepare(t, cfg)
			o := harness.AssertOutcome(t, outs, "mongodb", false)
			t.Logf("%s: source=%s template=%s", c.name, o.SourceDB, o.TemplateName)

			// Confirm both seeded collections landed in the
			// per-worktree DB (mongorestore remapped them).
			n := countDocs(t, o.SourceDB, "products")
			if n != 3 {
				t.Errorf("%s.products = %d docs, want 3", o.SourceDB, n)
			}
			n = countDocs(t, o.SourceDB, "orders")
			if n != 2 {
				t.Errorf("%s.orders = %d docs, want 2", o.SourceDB, n)
			}
		})
	}
}

func seed(t *testing.T) {
	t.Helper()
	script := `db = db.getSiblingDB('` + seedDB + `');
db.dropDatabase();
db = db.getSiblingDB('` + seedDB + `');
db.products.insertMany([{name:'a'},{name:'b'},{name:'c'}]);
db.orders.insertMany([{qty:1},{qty:2}]);`
	cmd := exec.Command("docker", "exec", "-i", mongoContainer,
		"mongosh", "--quiet")
	cmd.Stdin = bytes.NewReader([]byte(script))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func mongodump(t *testing.T, outPath string, gzip bool) {
	t.Helper()
	args := []string{"--uri=" + mongoURI, "--db=" + seedDB, "--archive=" + outPath}
	if gzip {
		args = append(args, "--gzip")
	}
	cmd := exec.Command("mongodump", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("mongodump: %v", err)
	}
}

func dropSeedDB(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(ctx)
	if err := c.Database(seedDB).Drop(ctx); err != nil {
		t.Fatalf("drop seed: %v", err)
	}
}

func countDocs(t *testing.T, dbName, collection string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Disconnect(ctx)
	n, err := c.Database(dbName).Collection(collection).CountDocuments(ctx, struct{}{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return int(n)
}
