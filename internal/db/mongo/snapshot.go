// Snapshot primitives for the MongoDB driver. Mirrors the MySQL +
// Postgres `SnapshotCreate` / `SnapshotRestore` shape so the prepare
// orchestrator can treat Mongo with the same cache-hit / cold-build
// flow.
//
// Implementation: per-collection `$out` aggregation copies documents
// to the destination database; index definitions are then copied via
// listIndexes + createIndexes. No subprocess dependencies on
// mongodump / mongorestore — all wire-level work goes through the
// existing v2 driver.

package mongo

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/sync/errgroup"
)

// EngineVersion returns the buildInfo.version field for the
// connected cluster — used in the snapshot fingerprint so a server
// upgrade invalidates cached templates.
func (d *Driver) EngineVersion(ctx context.Context) (string, error) {
	res := d.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}})
	if err := res.Err(); err != nil {
		return "", err
	}
	var out struct {
		Version string `bson:"version"`
	}
	if err := res.Decode(&out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// DatabaseExists reports whether `name` is in the cluster's database
// list. Used by the cache-hit path to confirm a cached template DB
// is still live in mongo (the SQLite snapshot row only proves it was
// created at some point, not that it still exists).
func (d *Driver) DatabaseExists(ctx context.Context, name string) (bool, error) {
	names, err := d.Client.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: name}})
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// SnapshotCreate copies every collection (data + indexes) from
// `source` into a freshly-created `template` database. Idempotent —
// pre-existing `template` is dropped first. Index creation is best-
// effort: if the underlying collection had ad-hoc indexes that the
// destination already auto-creates (e.g. `_id_`), the duplicate-key
// is swallowed.
func (d *Driver) SnapshotCreate(ctx context.Context, source, template string) error {
	if source == template {
		return fmt.Errorf("snapshot create: source and template must differ (%q)", source)
	}
	if err := d.Client.Database(template).Drop(ctx); err != nil {
		return fmt.Errorf("drop template %s: %w", template, err)
	}
	return d.cloneDatabase(ctx, source, template)
}

// SnapshotRestore copies `template` → `target`. Idempotent: any
// pre-existing target is dropped first. Mirrors SnapshotCreate in
// the opposite direction.
func (d *Driver) SnapshotRestore(ctx context.Context, template, target string) error {
	if template == target {
		return fmt.Errorf("snapshot restore: template and target must differ (%q)", template)
	}
	if err := d.Client.Database(target).Drop(ctx); err != nil {
		return fmt.Errorf("drop target %s: %w", target, err)
	}
	return d.cloneDatabase(ctx, template, target)
}

// DropSnapshot drops the named template database. Used by the
// snapshot GC sweep.
func (d *Driver) DropSnapshot(ctx context.Context, template string) error {
	return d.Client.Database(template).Drop(ctx)
}

// cloneDatabase is the shared inner loop for SnapshotCreate /
// SnapshotRestore: list source collections, fan out the per-
// collection clones (limit 6 — same as DropMatching), copy indexes.
func (d *Driver) cloneDatabase(ctx context.Context, source, dest string) error {
	srcDB := d.Client.Database(source)
	colls, err := srcDB.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return fmt.Errorf("list collections in %s: %w", source, err)
	}
	if len(colls) == 0 {
		// Empty source — create the dest database by inserting +
		// dropping a sentinel doc. Mongo doesn't have an explicit
		// "create database" op; lazy creation on first write is
		// the documented pattern.
		sentinel := d.Client.Database(dest).Collection("_treeman_init")
		if _, err := sentinel.InsertOne(ctx, bson.D{{Key: "_treeman", Value: 1}}); err != nil {
			return fmt.Errorf("seed empty dest %s: %w", dest, err)
		}
		_ = sentinel.Drop(ctx)
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(6)
	for _, coll := range colls {
		coll := coll
		g.Go(func() error {
			return d.cloneCollection(gctx, source, dest, coll)
		})
	}
	return g.Wait()
}

// cloneCollection copies one collection's documents (via `$out`) and
// then its index definitions (via listIndexes + createIndexes).
// `$out` already creates the destination collection; index creation
// then layers user-defined indexes on top.
func (d *Driver) cloneCollection(ctx context.Context, source, dest, coll string) error {
	srcDB := d.Client.Database(source)
	pipeline := mongo.Pipeline{
		bson.D{{
			Key: "$out", Value: bson.D{
				{Key: "db", Value: dest},
				{Key: "coll", Value: coll},
			},
		}},
	}
	cur, err := srcDB.Collection(coll).Aggregate(ctx, pipeline)
	if err != nil {
		return fmt.Errorf("$out %s.%s → %s.%s: %w", source, coll, dest, coll, err)
	}
	if err := cur.Close(ctx); err != nil {
		return fmt.Errorf("close $out cursor for %s.%s: %w", source, coll, err)
	}
	return copyIndexes(ctx, srcDB.Collection(coll), d.Client.Database(dest).Collection(coll))
}

// copyIndexes reads the source's listIndexes output and re-creates
// each non-`_id_` index on the destination. `_id_` is auto-created
// by mongo on any new collection, so re-creating it surfaces an
// IndexOptionsConflict-shaped error we silently drop.
func copyIndexes(ctx context.Context, src, dst *mongo.Collection) error {
	cur, err := src.Indexes().List(ctx)
	if err != nil {
		return fmt.Errorf("list indexes: %w", err)
	}
	defer cur.Close(ctx)

	var models []mongo.IndexModel
	for cur.Next(ctx) {
		var idx bson.M
		if err := cur.Decode(&idx); err != nil {
			return fmt.Errorf("decode index: %w", err)
		}
		name, _ := idx["name"].(string)
		if name == "_id_" {
			// Auto-created by mongo on collection birth; re-creating
			// it triggers IndexOptionsConflict.
			continue
		}
		keys, ok := idx["key"]
		if !ok {
			continue
		}
		// Strip server-side bookkeeping fields before round-tripping
		// the index back through createIndexes.
		opts := options.Index()
		if name != "" {
			opts.SetName(name)
		}
		if v, ok := idx["unique"].(bool); ok && v {
			opts.SetUnique(true)
		}
		if v, ok := idx["sparse"].(bool); ok && v {
			opts.SetSparse(true)
		}
		if v, ok := idx["partialFilterExpression"].(bson.M); ok {
			opts.SetPartialFilterExpression(v)
		}
		if v, ok := idx["expireAfterSeconds"].(int32); ok {
			opts.SetExpireAfterSeconds(v)
		}
		models = append(models, mongo.IndexModel{Keys: keys, Options: opts})
	}
	if len(models) == 0 {
		return nil
	}
	if _, err := dst.Indexes().CreateMany(ctx, models); err != nil {
		// Conflict on a pre-existing identical index is fine.
		if !isIndexConflict(err) {
			return fmt.Errorf("createIndexes on dst: %w", err)
		}
	}
	return nil
}

// isIndexConflict reports whether `err` is the "index already
// exists with the same name and options" CommandError code 85.
func isIndexConflict(err error) bool {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return ce.Code == 85 || ce.Code == 86 // IndexOptionsConflict / IndexKeySpecsConflict
	}
	return false
}
