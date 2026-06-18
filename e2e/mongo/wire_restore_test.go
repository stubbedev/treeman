//go:build e2e

package mongo_e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/dumpload"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
)

const wireURI = "mongodb://127.0.0.1:27117"

// TestWireRestoreFidelityAndAtomicity exercises the pure-Go wire
// fallback (no mongorestore on PATH, no ContainerRef) against a real
// mongod:
//
//  1. fidelity: docs land batched, secondary indexes replay from the
//     prelude metadata, prelude-only (empty) collections are created,
//     and the source→target DB remap applies
//  2. atomicity: a truncated archive fails the restore WITHOUT
//     touching pre-existing target collections, and leaves no
//     `_tmload_` staging junk behind
func TestWireRestoreFidelityAndAtomicity(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))
	waitForMongo(t)

	// Force strategy 3: empty PATH hides mongorestore, and the conn has
	// no ContainerRef, so docker-exec is skipped too.
	t.Setenv("PATH", t.TempDir())
	conn := &config.MongoConn{URI: wireURI}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(wireURI))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	target := client.Database("treeman_wire_target")
	t.Cleanup(func() { _ = target.Drop(context.Background()) })

	// ── fidelity ──
	dump := filepath.Join(t.TempDir(), "seed.archive")
	if err := os.WriteFile(dump, buildArchive(t, false), 0o644); err != nil {
		t.Fatal(err)
	}
	strategy, err := dbmongo.Restore(ctx, conn, "treeman_wire_target", "srcdb", dump)
	if err != nil {
		t.Fatalf("wire restore: %v", err)
	}
	if strategy != dumpload.StrategyWire {
		t.Fatalf("strategy = %s, want %s (fast paths must be unavailable)", strategy, dumpload.StrategyWire)
	}
	n, err := target.Collection("users").CountDocuments(ctx, bson.D{})
	if err != nil || n != 3 {
		t.Fatalf("users count = %d, %v; want 3", n, err)
	}
	assertIndexExists(t, ctx, target.Collection("users"), "email_1")
	// Prelude-only collection exists, empty.
	names, err := target.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(names, "audit_log") {
		t.Errorf("empty prelude collection audit_log missing; have %v", names)
	}
	assertCapped(t, ctx, target, "audit_log")
	for _, c := range names {
		if len(c) >= 8 && c[:8] == "_tmload_" {
			t.Errorf("staging junk survived successful restore: %s", c)
		}
	}

	// ── atomicity ──
	// Re-restore from a TRUNCATED archive: the existing users docs must
	// survive, and no staging collections may linger.
	trunc := buildArchive(t, false)
	trunc = trunc[:len(trunc)-12] // chop the trailing terminators + part of a doc
	dump2 := filepath.Join(t.TempDir(), "broken.archive")
	if err := os.WriteFile(dump2, trunc, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := dbmongo.Restore(ctx, conn, "treeman_wire_target", "srcdb", dump2); err == nil {
		t.Fatal("truncated archive: want restore error")
	}
	n, err = target.Collection("users").CountDocuments(ctx, bson.D{})
	if err != nil || n != 3 {
		t.Fatalf("users count after failed restore = %d, %v; want 3 (target untouched)", n, err)
	}
	assertIndexExists(t, ctx, target.Collection("users"), "email_1")
	names, err = target.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range names {
		if len(c) >= 8 && c[:8] == "_tmload_" {
			t.Errorf("staging junk survived failed restore: %s", c)
		}
	}
}

// buildArchive constructs a minimal-but-real `mongodump --archive`
// byte stream: prelude metadata for srcdb.users (with a secondary
// index) + srcdb.audit_log (no body docs), then a users body block
// with 3 documents.
func buildArchive(t *testing.T, gzipped bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	w32 := func(v uint32) {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	doc := func(v any) {
		raw, err := bson.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(raw)
	}
	w32(0x8199e26d) // archive magic
	doc(bson.M{"concurrent_collections": int32(1), "server_version": "7.0.0"})
	doc(bson.M{
		"db":         "srcdb",
		"collection": "users",
		"metadata":   `{"indexes":[{"v":2,"key":{"_id":1},"name":"_id_"},{"v":2,"key":{"email":1},"name":"email_1","unique":true}],"type":"collection"}`,
	})
	doc(bson.M{
		"db": "srcdb", "collection": "audit_log",
		"metadata": `{"indexes":[{"v":2,"key":{"_id":1},"name":"_id_"}],"options":{"capped":true,"size":4096},"type":"collection"}`,
	})
	w32(0xFFFFFFFF) // end of prelude
	doc(bson.M{"db": "srcdb", "collection": "users"})
	doc(bson.M{"_id": int32(1), "email": "a@x"})
	doc(bson.M{"_id": int32(2), "email": "b@x"})
	doc(bson.M{"_id": int32(3), "email": "c@x"})
	w32(0xFFFFFFFF) // end of users block
	doc(bson.M{"db": "srcdb", "collection": "audit_log", "EOF": true})
	w32(0xFFFFFFFF)
	w32(0xFFFFFFFF) // end of archive
	return buf.Bytes()
}

func assertIndexExists(t *testing.T, ctx context.Context, coll *mongo.Collection, name string) {
	t.Helper()
	cur, err := coll.Indexes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close(ctx) }()
	for cur.Next(ctx) {
		var idx struct {
			Name string `bson:"name"`
		}
		if err := cur.Decode(&idx); err != nil {
			t.Fatal(err)
		}
		if idx.Name == name {
			return
		}
	}
	t.Errorf("index %s missing on %s", name, coll.Name())
}

// assertCapped verifies a collection's options replayed: listCollections
// must report capped:true for it.
func assertCapped(t *testing.T, ctx context.Context, db *mongo.Database, coll string) {
	t.Helper()
	cur, err := db.ListCollections(ctx, bson.D{{Key: "name", Value: coll}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close(ctx) }()
	for cur.Next(ctx) {
		var info struct {
			Options struct {
				Capped bool `bson:"capped"`
			} `bson:"options"`
		}
		if err := cur.Decode(&info); err != nil {
			t.Fatal(err)
		}
		if !info.Options.Capped {
			t.Errorf("%s: capped option not replayed", coll)
		}
		return
	}
	t.Errorf("%s not found in listCollections", coll)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func waitForMongo(t *testing.T) {
	t.Helper()
	harness.WaitForReady(t, "mongo:27117", 60*time.Second, func() error {
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c, err := mongo.Connect(options.Client().ApplyURI(wireURI))
		if err != nil {
			return err
		}
		defer func() { _ = c.Disconnect(pingCtx) }()
		return c.Ping(pingCtx, nil)
	})
}
