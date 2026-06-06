//go:build e2e

// Package mongo_dump_e2e exercises treeman's real mongorestore path
// (vs the seed-script path the mongo/ suite uses). Sequence:
//
//  1. boot mongo container
//  2. seed source data via mongosh
//  3. mongodump --archive into a fixture file
//  4. drop the seeded data
//  5. point treeman's prepare at the archive
//  6. confirm the rehydrated DB has the expected docs
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
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

const (
	mongoURI       = "mongodb://127.0.0.1:27128"
	mongoContainer = "treeman-e2e-mongodump"
	seedDB         = "seed_source"
)

func TestMongoArchiveRestore(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("mongorestore"); err != nil {
		t.Skip("mongorestore not on PATH")
	}
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mongo:27128", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:27128", 1*time.Second)
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
						Dump: config.DumpList{{
							Path:     "dump.archive",
							SourceDB: seedDB,
						}},
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

// TestMongoArchiveDockerExec proves the mongo dump-load dispatcher
// picks the docker-exec fast path when ContainerRef is set — running
// mongorestore INSIDE the mongo container. Asserts strategy=docker-exec
// on the dump-load phase event and that the rehydrated collections
// carry the expected counts.
func TestMongoArchiveDockerExec(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("mongorestore"); err != nil {
		t.Skip("mongorestore not on PATH (needed only to GENERATE the test archive)")
	}
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mongo:27128", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:27128", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	seed(t)
	archivePath := filepath.Join(t.TempDir(), "seed.archive")
	mongodump(t, archivePath, false)
	dropSeedDB(t)

	wt := t.TempDir()
	body, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(wt, "dump.archive")
	if err := os.WriteFile(fixturePath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mongodb: &config.MongoConn{
				// In-container URI; containerip rewrites host (and the
				// driver picks up port 27017 from inside the bridge).
				URI: "mongodb://127.0.0.1:27017",
				ContainerRef: config.ContainerRef{
					Container: mongoContainer,
				},
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mongodb",
			NameTemplate: "tm_mgdx_{slug}",
			Dump:         config.DumpList{{Path: "dump.archive", SourceDB: seedDB}},
		}},
	}
	env := harness.NewEnv(t, wt)
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "mongodb", false)
	t.Logf("docker-exec: source=%s template=%s", o.SourceDB, o.TemplateName)

	evs, err := env.Store.QueryEvents(env.Ctx, store.EventFilter{
		WorktreeID: env.WTID,
		EventTypes: []string{"prepare:phase"},
		Phases:     []string{"dump-load"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawDockerExec bool
	for _, e := range evs {
		t.Logf("phase event: %s", e.Message)
		if strings.Contains(e.Message, "strategy=docker-exec") {
			sawDockerExec = true
		}
	}
	if !sawDockerExec {
		t.Errorf("expected strategy=docker-exec — ContainerRef should have selected the fast path")
	}
	if n := countDocs(t, o.SourceDB, "products"); n != 3 {
		t.Errorf("products docs = %d, want 3", n)
	}
	if n := countDocs(t, o.SourceDB, "orders"); n != 2 {
		t.Errorf("orders docs = %d, want 2", n)
	}
}

// TestMongoArchiveWireFallback proves the Go-native BSON archive parser
// + driver-insert fallback completes a cold build when mongorestore is
// NOT on PATH and ContainerRef isn't set. Generates the archive with
// mongorestore (still on PATH at that point), then narrows PATH to a
// directory holding only docker (for compose teardown) so the dispatch
// has no fast path available.
func TestMongoArchiveWireFallback(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("mongorestore"); err != nil {
		t.Skip("mongorestore not on PATH (needed only to GENERATE the test archive)")
	}
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mongo:27128", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:27128", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	seed(t)
	archivePath := filepath.Join(t.TempDir(), "seed.archive")
	mongodump(t, archivePath, false)
	dropSeedDB(t)

	wt := t.TempDir()
	fixturePath := filepath.Join(wt, "dump.archive")
	body, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	// Narrow PATH so the dispatcher's docker-exec + native-CLI paths
	// both skip and the wire fallback runs. We still need docker
	// available for the compose teardown, so symlink it (and sh) into
	// the sandbox dir; mongorestore is intentionally absent.
	sandbox := t.TempDir()
	for _, bin := range []string{"docker", "sh"} {
		full, perr := exec.LookPath(bin)
		if perr != nil {
			continue
		}
		_ = os.Symlink(full, filepath.Join(sandbox, bin))
	}
	t.Setenv("PATH", sandbox)
	if _, err := exec.LookPath("mongorestore"); err == nil {
		t.Fatal("PATH sandbox leak: mongorestore still resolvable")
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mongodb: &config.MongoConn{URI: mongoURI},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "mongodb",
			NameTemplate: "tm_mgw_{slug}",
			Dump:         config.DumpList{{Path: "dump.archive", SourceDB: seedDB}},
		}},
	}
	env := harness.NewEnv(t, wt)
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "mongodb", false)
	t.Logf("wire fallback: source=%s template=%s", o.SourceDB, o.TemplateName)

	evs, err := env.Store.QueryEvents(env.Ctx, store.EventFilter{
		WorktreeID: env.WTID,
		EventTypes: []string{"prepare:phase"},
		Phases:     []string{"dump-load"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("no dump-load prepare_phase events recorded")
	}
	var sawWire bool
	for _, e := range evs {
		t.Logf("phase event: %s", e.Message)
		if strings.Contains(e.Message, "strategy=wire") {
			sawWire = true
		}
	}
	if !sawWire {
		t.Errorf("expected strategy=wire (PATH sandbox stripped mongorestore + no ContainerRef) — fast path leaked through")
	}

	if n := countDocs(t, o.SourceDB, "products"); n != 3 {
		t.Errorf("products docs = %d, want 3 (BSON archive parser or driver insert lost a row)", n)
	}
	if n := countDocs(t, o.SourceDB, "orders"); n != 2 {
		t.Errorf("orders docs = %d, want 2", n)
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
