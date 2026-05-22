// Package prepare orchestrates the per-worktree bring-up: ensure
// the source DB exists, optionally load a dump, run the framework's
// migrate command, snapshot the source for cache reuse, fan out
// into N paratest clone DBs.
//
// All filesystem reads (migration files, dump file, lockfiles) and
// the framework-migrate cwd resolve against the WORKTREE root. Each
// linked worktree carries its own branch checkout, so migrations and
// dumps land in the worktree's own copy; treating them as worktree-
// scoped is the only way edits inside a worktree propagate into the
// scoped source DB + clones.
package prepare

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"

	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/dumpload"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/migrations/runner"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// frameworkHashCache adapts *store.Store to framework.HashCache.
// store.BatchDirHashes / UpsertDirHashes use store-defined types
// (DirHashRecord, DirHashKey) that the framework package can't
// import (store depends on framework's caller chain, not the other
// way round). Field shapes match exactly, so a single struct
// conversion bridges the boundary at zero allocation cost.
type frameworkHashCache struct{ *store.Store }

func (f frameworkHashCache) BatchDirHashes(ctx context.Context, dirs []string, specName, hashMode string) (map[string]framework.DirHashCacheRecord, error) {
	raw, err := f.Store.BatchDirHashes(ctx, dirs, specName, hashMode)
	if err != nil {
		return nil, err
	}
	out := make(map[string]framework.DirHashCacheRecord, len(raw))
	for k, v := range raw {
		out[k] = framework.DirHashCacheRecord(v)
	}
	return out, nil
}

func (f frameworkHashCache) UpsertDirHashes(ctx context.Context, entries []framework.DirHashCacheKey, hashes map[string]string) error {
	se := make([]store.DirHashKey, len(entries))
	for i, e := range entries {
		se[i] = store.DirHashKey(e)
	}
	return f.Store.UpsertDirHashes(ctx, se, hashes)
}

// Outcome — one row per database the orchestrator handled.
type Outcome struct {
	Engine       string
	SourceDB     string
	TemplateName string
	Fingerprint  string
	CacheHit     bool
	Clones       []string
}

// cloneRestorer is the engine-specific `SnapshotRestore` signature
// — both the mysql and postgres drivers expose it identically.
// Letting fanOutClones take this as a func avoids tying the helper
// to a concrete driver type.
type cloneRestorer func(ctx context.Context, template, target string) error

// fanOutLimits caps the outer concurrency of fanOutClones per
// engine. MySQL's SnapshotRestore itself uses up to ~6 inner
// connections (parallel table copy), so the outer fan-out is held
// to 4 to keep the total under a typical 100-conn ceiling. Postgres
// SnapshotRestore is a single `CREATE DATABASE … TEMPLATE`
// statement, so the outer fan-out can use the whole CPU budget.
var fanOutLimits = map[string]int{
	"mysql":      4,
	"mariadb":    4,
	"tidb":       4,
	"postgres":   0, // 0 = GOMAXPROCS
	"postgresql": 0,
}

// fanOutClones restores `template` into each of `clones` in
// parallel. Each restore is an engine-side operation (CREATE
// DATABASE … TEMPLATE on postgres, the parallel table copy path on
// mysql); the per-engine entry in fanOutLimits caps outer
// concurrency so a 32-core box can't overrun the DB's connection
// ceiling.
//
// `override` (>0) replaces the default limit — sourced from the
// `databases[].fanout` YAML field. Users who've raised
// max_connections (or who run a beefier PG that doesn't contend on
// pg_database) can opt into higher concurrency without recompiling.
//
// Returns the first error seen. errgroup's ctx cancellation
// propagates so peer restores abort instead of hammering a server
// that already refused the first one.
func fanOutClones(ctx context.Context, restore cloneRestorer, template string, clones []string, engine string, override uint32) error {
	if len(clones) == 0 {
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	limit := 0
	if override > 0 {
		limit = int(override)
	} else {
		limit = fanOutLimits[engine]
		if limit == 0 {
			limit = runtime.GOMAXPROCS(0)
		}
	}
	if limit < 2 {
		limit = 2
	}
	if limit > len(clones) {
		limit = len(clones)
	}
	g.SetLimit(limit)
	for _, c := range clones {
		c := c
		g.Go(func() error {
			if err := restore(gctx, template, c); err != nil {
				return fmt.Errorf("restore %s → %s: %w", template, c, err)
			}
			return nil
		})
	}
	return g.Wait()
}

// Run drives prepare for every database declared by cfg.Databases.
//
// `worktreePath` is the linked-worktree checkout root — migration
// files, dump file, lockfiles, and the framework migrate command all
// resolve there. The slug used in template rendering also derives
// from this worktree.
//
// Engines run in parallel — each engine prepare touches a different
// driver pool (mysql, postgres, mongo, redis, ES) so they don't
// contend for the same connections. The slowest engine sets the
// wall-clock; previously a slow mysql cold-build serialized in
// front of a 200ms redis flush.
func Run(
	ctx context.Context,
	cfg *config.Config,
	worktreePath string,
	sl slug.Slug,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) ([]Outcome, error) {
	tplCtx := template.FromSlug(sl)

	results := make([]Outcome, len(cfg.Databases))
	hasResult := make([]bool, len(cfg.Databases))
	g, gctx := errgroup.WithContext(ctx)
	for i, d := range cfg.Databases {
		i, d := i, d
		g.Go(func() error {
			var (
				o   Outcome
				err error
			)
			switch d.Engine {
			case "mysql", "mariadb", "tidb":
				o, err = prepareMySQL(gctx, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			case "postgres", "postgresql":
				o, err = preparePostgres(gctx, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			case "mongodb":
				o, err = prepareMongo(gctx, cfg, d, tplCtx, st, repoID, worktreeID)
			case "redis":
				o, err = prepareRedis(gctx, cfg, d, tplCtx, st, repoID, worktreeID)
			case "elasticsearch", "opensearch":
				o, err = prepareES(gctx, cfg, d, tplCtx, st, repoID, worktreeID)
			default:
				// Engine not recognised. Surface via event so the
				// user notices, but don't fail the whole prepare run.
				_ = st.WriteEvent(gctx, store.LevelWarn, "prepare_unsupported_engine",
					fmt.Sprintf("engine=%s not recognised", d.Engine),
					repoID, worktreeID, "", 0, nil)
				return nil
			}
			if err != nil {
				return err
			}
			results[i] = o
			hasResult[i] = true
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		// Return whatever outcomes did land so callers can log a
		// partial-success picture before surfacing the error.
		var outcomes []Outcome
		for i, ok := range hasResult {
			if ok {
				outcomes = append(outcomes, results[i])
			}
		}
		return outcomes, err
	}
	var outcomes []Outcome
	for i, ok := range hasResult {
		if ok {
			outcomes = append(outcomes, results[i])
		}
	}
	return outcomes, nil
}

func prepareMySQL(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	if cfg.Connections.Mysql == nil {
		return Outcome{}, fmt.Errorf("connections.mysql not configured")
	}
	sourceDB, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}

	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		return Outcome{}, err
	}
	defer drv.Close()

	version, _ := drv.EngineVersion(ctx)

	// Build fingerprint inputs.
	migrationsHash := ""
	dumpHash := ""
	lockfileHashes := map[string]string{}
	frameworkName := ""
	hashMode := ""
	if d.Migrations != nil {
		frameworkName = d.Migrations.Framework
		s := specFromYAML(*d.Migrations)
		hashMode = string(s.HashMode)
		if len(s.MigrationDirs) > 0 || len(s.FileGlobs) > 0 {
			if h, err := framework.MigrationsHashWithCache(ctx, frameworkHashCache{st}, worktreePath, s); err == nil {
				migrationsHash = h
			}
		}
		lockPaths := make([]string, 0, len(s.Lockfiles))
		for _, lf := range s.Lockfiles {
			lockPaths = append(lockPaths, filepath.Join(worktreePath, lf))
		}
		if len(lockPaths) > 0 {
			if h, err := snapshot.LockfileHashesForWithCache(ctx, st, lockPaths); err == nil {
				lockfileHashes = h
			}
		}
	}
	if d.Dump != nil {
		dp := filepath.Join(worktreePath, d.Dump.Path)
		hashes, _ := snapshot.LockfileHashesForWithCache(ctx, st, []string{dp})
		dumpHash = hashes[filepath.Base(dp)]
	}

	key := snapshot.New(d.Engine, version, sourceDB, frameworkName, hashMode, migrationsHash, dumpHash, lockfileHashes)
	templateName := key.TemplateName()

	// Cache lookup: if SQLite knows a snapshot row for this
	// fingerprint AND the template DB still exists in MySQL, skip
	// the cold build and just clone the template into paratest DBs.
	if rec, err := st.LookupSnapshot(ctx, key.Fingerprint()); err == nil && rec != nil {
		if exists, _ := drv.DatabaseExists(ctx, rec.TemplateName); exists {
			_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_hit",
				fmt.Sprintf("template=%s", rec.TemplateName),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":      d.Engine,
					"source_db":   sourceDB,
					"template":    rec.TemplateName,
					"fingerprint": key.Fingerprint(),
				})
			clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
			if err != nil {
				return Outcome{}, err
			}
			if err := fanOutClones(ctx, drv.SnapshotRestore, rec.TemplateName, clones, d.Engine, d.Fanout); err != nil {
				return Outcome{}, err
			}
			_ = st.TouchSnapshot(ctx, key.Fingerprint())
			_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
				fmt.Sprintf("cache_hit clones=%d", len(clones)),
				repoID, worktreeID, "", 0, map[string]string{
					"source_db":   sourceDB,
					"template":    rec.TemplateName,
					"clones":      fmt.Sprintf("%d", len(clones)),
					"fingerprint": key.Fingerprint(),
				})
			return Outcome{
				Engine:       d.Engine,
				SourceDB:     sourceDB,
				TemplateName: rec.TemplateName,
				Fingerprint:  key.Fingerprint(),
				CacheHit:     true,
				Clones:       clones,
			}, nil
		}
		// Row stale (template was dropped externally). Wipe so the
		// cold-build path below overwrites it cleanly.
		_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	}

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=mysql source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "mysql",
			"source_db":   sourceDB,
			"template":    templateName,
			"fingerprint": key.Fingerprint(),
		})

	// Cold build: drop+recreate source, load dump, run migrate,
	// snapshot for cache, fan out paratest clones.
	if _, err := drv.DropMatching(ctx, sourceDB); err != nil {
		return Outcome{}, err
	}
	if err := drv.EnsureDB(ctx, sourceDB); err != nil {
		return Outcome{}, err
	}
	if d.Dump != nil {
		dp := filepath.Join(worktreePath, d.Dump.Path)
		if _, err := dumpload.LoadMySQL(ctx, drv.DB, sourceDB, dp); err != nil {
			return Outcome{}, fmt.Errorf("load dump %s: %w", dp, err)
		}
	}
	if d.Migrations != nil {
		out, err := runner.Run(ctx, frameworkName, worktreePath, sourceDB, runner.ModeUp, nil, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("migrate source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("migrate source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
	}

	// Build the template snapshot, then clone it into paratest DBs.
	if err := drv.SnapshotCreate(ctx, sourceDB, templateName); err != nil {
		return Outcome{}, fmt.Errorf("snapshot create %s → %s: %w", sourceDB, templateName, err)
	}
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    key.Fingerprint(),
		Engine:         d.Engine,
		EngineVersion:  version,
		SourceDB:       sourceDB,
		TemplateName:   templateName,
		MigrationsHash: migrationsHash,
		DumpHash:       dumpHash,
		LockfileHashes: lockfileHashes,
		RepoID:         repoID,
	})
	// Fire-and-forget LRU eviction for this repo. Uses a fresh
	// background context so the goroutine outlives the prepare call
	// even when the foreground request is cancelled. Errors are
	// logged inside EvictExcess.
	go snapshot.EvictExcess(context.Background(), cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, drv.SnapshotRestore, templateName, clones, d.Engine, d.Fanout); err != nil {
		return Outcome{}, err
	}

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("clones=%d", len(clones)),
		repoID, worktreeID, "", 0, map[string]string{
			"source_db": sourceDB,
			"template":  templateName,
			"clones":    fmt.Sprintf("%d", len(clones)),
		})

	return Outcome{
		Engine:       d.Engine,
		SourceDB:     sourceDB,
		TemplateName: templateName,
		Fingerprint:  key.Fingerprint(),
		CacheHit:     false,
		Clones:       clones,
	}, nil
}

// preparePostgres mirrors prepareMySQL for the PostgreSQL engine.
// Uses `CREATE DATABASE … TEMPLATE` as the snapshot-and-fan-out
// primitive — fast because pg copies on-disk files instead of
// replaying SQL. Cache hit path identical to MySQL: SQLite
// fingerprint lookup + pg_database existence check.
func preparePostgres(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	if cfg.Connections.Postgres == nil {
		return Outcome{}, fmt.Errorf("connections.postgres not configured")
	}
	sourceDB, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}

	drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
	if err != nil {
		return Outcome{}, err
	}
	defer drv.Close()

	version, _ := drv.EngineVersion(ctx)

	migrationsHash := ""
	dumpHash := ""
	lockfileHashes := map[string]string{}
	frameworkName := ""
	hashMode := ""
	if d.Migrations != nil {
		frameworkName = d.Migrations.Framework
		s := specFromYAML(*d.Migrations)
		hashMode = string(s.HashMode)
		if len(s.MigrationDirs) > 0 || len(s.FileGlobs) > 0 {
			if h, err := framework.MigrationsHashWithCache(ctx, frameworkHashCache{st}, worktreePath, s); err == nil {
				migrationsHash = h
			}
		}
		lockPaths := make([]string, 0, len(s.Lockfiles))
		for _, lf := range s.Lockfiles {
			lockPaths = append(lockPaths, filepath.Join(worktreePath, lf))
		}
		if len(lockPaths) > 0 {
			if h, err := snapshot.LockfileHashesForWithCache(ctx, st, lockPaths); err == nil {
				lockfileHashes = h
			}
		}
	}
	if d.Dump != nil {
		dp := filepath.Join(worktreePath, d.Dump.Path)
		hashes, _ := snapshot.LockfileHashesForWithCache(ctx, st, []string{dp})
		dumpHash = hashes[filepath.Base(dp)]
	}

	key := snapshot.New(d.Engine, version, sourceDB, frameworkName, hashMode, migrationsHash, dumpHash, lockfileHashes)
	templateName := key.TemplateName()

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=postgres source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "postgres",
			"source_db":   sourceDB,
			"template":    templateName,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit?
	if rec, err := st.LookupSnapshot(ctx, key.Fingerprint()); err == nil && rec != nil {
		if exists, _ := drv.DatabaseExists(ctx, rec.TemplateName); exists {
			_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_hit",
				fmt.Sprintf("template=%s", rec.TemplateName),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":      "postgres",
					"source_db":   sourceDB,
					"template":    rec.TemplateName,
					"fingerprint": key.Fingerprint(),
				})
			clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
			if err != nil {
				return Outcome{}, err
			}
			if err := fanOutClones(ctx, drv.SnapshotRestore, rec.TemplateName, clones, d.Engine, d.Fanout); err != nil {
				return Outcome{}, err
			}
			_ = st.TouchSnapshot(ctx, key.Fingerprint())
			return Outcome{
				Engine: d.Engine, SourceDB: sourceDB,
				TemplateName: rec.TemplateName, Fingerprint: key.Fingerprint(),
				CacheHit: true, Clones: clones,
			}, nil
		}
		_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	}

	// Cold build.
	if _, err := drv.DropMatching(ctx, sourceDB); err != nil {
		return Outcome{}, err
	}
	if err := drv.EnsureDB(ctx, sourceDB); err != nil {
		return Outcome{}, err
	}
	if d.Dump != nil {
		dp := filepath.Join(worktreePath, d.Dump.Path)
		if _, err := dumpload.LoadPostgres(ctx, drv.DB, sourceDB, dp); err != nil {
			return Outcome{}, fmt.Errorf("load dump %s: %w", dp, err)
		}
	}
	if d.Migrations != nil {
		out, err := runner.Run(ctx, frameworkName, worktreePath, sourceDB, runner.ModeUp, nil, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("migrate source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("migrate source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
	}
	if err := drv.SnapshotCreate(ctx, sourceDB, templateName); err != nil {
		return Outcome{}, fmt.Errorf("snapshot create %s → %s: %w", sourceDB, templateName, err)
	}
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint: key.Fingerprint(), Engine: d.Engine, EngineVersion: version,
		SourceDB: sourceDB, TemplateName: templateName,
		MigrationsHash: migrationsHash, DumpHash: dumpHash, LockfileHashes: lockfileHashes,
		RepoID: repoID,
	})
	go snapshot.EvictExcess(context.Background(), cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, drv.SnapshotRestore, templateName, clones, d.Engine, d.Fanout); err != nil {
		return Outcome{}, err
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("clones=%d", len(clones)),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": "postgres", "source_db": sourceDB,
			"template": templateName, "clones": fmt.Sprintf("%d", len(clones)),
		})
	return Outcome{
		Engine: d.Engine, SourceDB: sourceDB, TemplateName: templateName,
		Fingerprint: key.Fingerprint(), CacheHit: false, Clones: clones,
	}, nil
}

// prepareMongo readies the per-worktree MongoDB database namespace.
// Mongo has no template/clone primitive worth caching for typical
// dev workloads (the snapshot story for mongo is `mongorestore`
// which is comparable to a cold re-load anyway), so the bringup
// path is a "drop-if-exists + ready for first write".
//
// Future: if d.Dump.Path is set, treeman could `mongorestore` from
// it. Skipped for now — most apps using mongo seed via app code.
func prepareMongo(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	st *store.Store,
	repoID, worktreeID int64,
) (Outcome, error) {
	if cfg.Connections.Mongodb == nil {
		return Outcome{}, fmt.Errorf("connections.mongodb not configured")
	}
	name, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		return Outcome{}, err
	}
	defer drv.Close(ctx)
	if _, err := drv.DropMatching(ctx, name); err != nil {
		return Outcome{}, fmt.Errorf("mongo drop %s: %w", name, err)
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("mongodb namespace %s ready", name),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": "mongodb", "source_db": name,
		})
	return Outcome{Engine: d.Engine, SourceDB: name}, nil
}

// prepareRedis FLUSHDBs the per-worktree Redis logical DB index so
// the worktree starts with an empty namespace. Redis "databases"
// are just 0–15 integer indices, so the slug template renders to a
// number (typically via `{slug_redis_index}`).
func prepareRedis(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	st *store.Store,
	repoID, worktreeID int64,
) (Outcome, error) {
	if cfg.Connections.Redis == nil {
		return Outcome{}, fmt.Errorf("connections.redis not configured")
	}
	if d.Namespaces == nil || d.Namespaces.DbIndexTemplate == "" {
		return Outcome{}, fmt.Errorf("redis: missing namespaces.db_index_template")
	}
	idxStr, err := template.Render(d.Namespaces.DbIndexTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render db_index_template: %w", err)
	}
	var idx uint8
	if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
		return Outcome{}, fmt.Errorf("redis db index parse %q: %w", idxStr, err)
	}
	drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
	if err != nil {
		return Outcome{}, err
	}
	defer drv.Close()
	if err := drv.FlushDB(ctx, idx); err != nil {
		return Outcome{}, fmt.Errorf("redis flushdb %d: %w", idx, err)
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("redis db%d flushed", idx),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": "redis", "source_db": fmt.Sprintf("db%d", idx),
		})
	return Outcome{Engine: d.Engine, SourceDB: fmt.Sprintf("db%d", idx)}, nil
}

// prepareES drops every Elasticsearch / OpenSearch index whose
// name starts with the per-worktree prefix, so the worktree starts
// with no stale indices. Index creation is left to the application
// (ES typically auto-creates on first write).
func prepareES(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	st *store.Store,
	repoID, worktreeID int64,
) (Outcome, error) {
	if cfg.Connections.Elasticsearch == nil {
		return Outcome{}, fmt.Errorf("connections.elasticsearch not configured")
	}
	if d.Namespaces == nil || d.Namespaces.IndexPrefixTemplate == "" {
		return Outcome{}, fmt.Errorf("elasticsearch: missing namespaces.index_prefix_template")
	}
	prefix, err := template.Render(d.Namespaces.IndexPrefixTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render index_prefix_template: %w", err)
	}
	drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := drv.DropMatching(ctx, prefix); err != nil {
		return Outcome{}, fmt.Errorf("es drop %s*: %w", prefix, err)
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("es prefix %s cleared", prefix),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": "elasticsearch", "source_db": prefix,
		})
	return Outcome{Engine: d.Engine, SourceDB: prefix}, nil
}

// specFromYAML builds a framework.Spec from the migrations: block in
// `.treeman.yaml`, merged with the built-in defaults for the named
// framework (if any). The merge rules:
//
//   - MigrationDirs / FileGlobs / Lockfiles are the dedup'd union of
//     the framework's defaults and the user's explicit entries (the
//     framework's defaults come first; user entries extend the set).
//   - HashMode and OnModify take the user's explicit value when set,
//     otherwise the framework default, otherwise the global default
//     (filename / rebuild).
//
// This lets `migrations: { framework: laravel }` work standalone —
// the snapshot fingerprint still picks up Laravel's canonical
// migration dirs, file globs, and lockfile — while a user who
// supplies extra dirs simply adds to the set instead of having to
// retype the framework boilerplate.
func specFromYAML(m config.MigrationSpec) framework.Spec {
	base, _ := framework.LookupBuiltin(m.Framework)
	hash := framework.HashMode(m.HashMode)
	if hash == "" {
		hash = base.HashMode
	}
	if hash == "" {
		hash = framework.HashFilename
	}
	onMod := framework.OnModify(m.OnModify)
	if onMod == "" {
		onMod = base.OnModify
	}
	if onMod == "" {
		onMod = framework.OnRebuild
	}
	return framework.Spec{
		Name:          m.Framework,
		MigrationDirs: mergeStringSets(base.MigrationDirs, m.MigrationDirs),
		FileGlobs:     mergeStringSets(base.FileGlobs, m.FileGlobs),
		Lockfiles:     mergeStringSets(base.Lockfiles, m.Lockfiles),
		HashMode:      hash,
		OnModify:      onMod,
	}
}

// mergeStringSets returns the order-preserving dedup'd union of the
// argument slices. `base` entries come first so the framework's
// canonical patterns lead the list, then `extra` entries that aren't
// already present.
func mergeStringSets(base, extra []string) []string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	out := make([]string, 0, len(base)+len(extra))
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, s := range base {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range extra {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func resolveCloneNames(p *config.TestClonesSpec, tplCtx template.Context, repoRoot string) ([]string, error) {
	if p == nil {
		return nil, nil
	}
	var n uint32
	if p.Clones.Auto {
		n = testfw.DetectedCloneCount(repoRoot)
		if n == 0 {
			n = testfw.NumCPUs()
		}
	} else {
		n = p.Clones.Fixed
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]string, 0, n)
	for i := uint32(1); i <= n; i++ {
		name, err := template.Render(p.NameTemplate, tplCtx.WithN(int(i)))
		if err != nil {
			return nil, fmt.Errorf("render paratest name_template: %w", err)
		}
		out = append(out, name)
	}
	return out, nil
}

// TeardownDatabases drops every per-worktree namespace declared by
// cfg.Databases. Errors per-engine are logged + swallowed so a
// missing redis doesn't block dropping mysql.
//
// Engines tear down in parallel: each one drives a different driver
// (mysql + mongo + redis + ES) so there's no shared connection pool
// to contend on, and a slow ES delete-by-prefix shouldn't gate the
// fast mysql DROP DATABASE.
func TeardownDatabases(
	ctx context.Context,
	cfg *config.Config,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	tplCtx := template.FromSlug(slug.Slug{Value: sl, Source: slug.SourceTicket})

	g, gctx := errgroup.WithContext(ctx)
	for _, d := range cfg.Databases {
		d := d
		g.Go(func() error {
			if err := teardownOne(gctx, cfg, d, tplCtx, sl, repoID, worktreeID, st); err != nil {
				_ = st.WriteEvent(gctx, store.LevelWarn, "db_teardown_error",
					err.Error(), repoID, worktreeID, "", 0, nil)
			}
			return nil
		})
	}
	return g.Wait()
}

func teardownOne(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	switch d.Engine {
	case "mysql", "mariadb", "tidb":
		if cfg.Connections.Mysql == nil {
			return fmt.Errorf("connections.mysql not configured")
		}
		drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
		if err != nil {
			return err
		}
		defer drv.Close()
		name, err := template.Render(d.NameTemplate, tplCtx)
		if err != nil {
			return err
		}
		dropped, err := drv.DropMatching(ctx, name)
		if err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
			fmt.Sprintf("mysql: %s (%d)", name, len(dropped)),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "mysql", "slug": sl, "target": name, "count": len(dropped),
			})
		return nil
	case "postgres", "postgresql":
		if cfg.Connections.Postgres == nil {
			return fmt.Errorf("connections.postgres not configured")
		}
		drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
		if err != nil {
			return err
		}
		defer drv.Close()
		name, err := template.Render(d.NameTemplate, tplCtx)
		if err != nil {
			return err
		}
		dropped, err := drv.DropMatching(ctx, name)
		if err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
			fmt.Sprintf("postgres: %s (%d)", name, len(dropped)),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "postgres", "slug": sl, "target": name, "count": len(dropped),
			})
		return nil
	case "mongodb":
		if cfg.Connections.Mongodb == nil {
			return fmt.Errorf("connections.mongodb not configured")
		}
		drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
		if err != nil {
			return err
		}
		defer drv.Close(ctx)
		name, err := template.Render(d.NameTemplate, tplCtx)
		if err != nil {
			return err
		}
		dropped, err := drv.DropMatching(ctx, name)
		if err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
			fmt.Sprintf("mongodb: %s (%d)", name, len(dropped)),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "mongodb", "slug": sl, "target": name, "count": len(dropped),
			})
		return nil
	case "redis":
		if cfg.Connections.Redis == nil {
			return fmt.Errorf("connections.redis not configured")
		}
		if d.Namespaces == nil {
			return fmt.Errorf("redis: missing namespaces.db_index_template")
		}
		drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
		if err != nil {
			return err
		}
		defer drv.Close()
		idxStr, err := template.Render(d.Namespaces.DbIndexTemplate, tplCtx)
		if err != nil {
			return err
		}
		var idx uint8
		_, err = fmt.Sscanf(idxStr, "%d", &idx)
		if err != nil {
			return fmt.Errorf("redis db index parse %q: %w", idxStr, err)
		}
		if err := drv.FlushDB(ctx, idx); err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_flush",
			fmt.Sprintf("redis: db%d", idx),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "redis", "slug": sl, "target": fmt.Sprintf("db%d", idx),
			})
		return nil
	case "elasticsearch", "opensearch":
		if cfg.Connections.Elasticsearch == nil {
			return fmt.Errorf("connections.elasticsearch not configured")
		}
		if d.Namespaces == nil {
			return fmt.Errorf("es: missing namespaces.index_prefix_template")
		}
		drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if err != nil {
			return err
		}
		prefix, err := template.Render(d.Namespaces.IndexPrefixTemplate, tplCtx)
		if err != nil {
			return err
		}
		dropped, err := drv.DropMatching(ctx, prefix)
		if err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
			fmt.Sprintf("elasticsearch: %s (%d)", prefix, len(dropped)),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "elasticsearch", "slug": sl, "target": prefix, "count": len(dropped),
			})
		return nil
	}
	return nil
}
