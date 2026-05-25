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
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"lukechampine.com/blake3"

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
	"github.com/stubbedev/treeman/internal/runid"
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

// emitPhaseDone writes a `prepare_phase` event tagged with the step
// name (dump-load, migrate, seed, snapshot-create) and the wall-clock
// duration measured from `stepStart`. Used by every per-engine
// prepare to give the user step-by-step visibility into where time
// is spent during a cold build. The run_id (set on ctx in the
// dispatch layer) is auto-injected into the payload by store.
func emitPhaseDone(ctx context.Context, st *store.Store, repoID, worktreeID int64,
	engine, sourceDB, phase string, stepStart time.Time) {
	if st == nil {
		return
	}
	durMs := time.Since(stepStart).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_phase",
		fmt.Sprintf("%s/%s phase=%s duration=%dms", engine, sourceDB, phase, durMs),
		repoID, worktreeID, phase, durMs, map[string]string{
			"engine":      engine,
			"source_db":   sourceDB,
			"duration_ms": fmt.Sprintf("%d", durMs),
		})
}

// dumpReady resolves d.Dump against worktreePath and reports
// whether the dump should be loaded. Returns (path, true, nil) when
// the file exists; (path, false, nil) when the file is missing AND
// d.Dump.Optional == true (caller should skip the load step);
// (path, false, err) when the file is required but missing.
// dump==nil short-circuits with ("", false, nil) so callers can
// branch on the bool alone.
func dumpReady(dump *config.DumpSpec, worktreePath string) (string, bool, error) {
	if dump == nil {
		return "", false, nil
	}
	p := filepath.Join(worktreePath, dump.Path)
	if _, err := os.Stat(p); err != nil {
		if dump.Optional {
			return p, false, nil
		}
		return p, false, fmt.Errorf("dump %s: %w", p, err)
	}
	return p, true, nil
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

// fanOutLimits is the conservative-default outer-concurrency cap per
// engine, used when neither an explicit `databases[].fanout` nor a
// connection-budget probe is available.
//
// MySQL's SnapshotRestore uses up to ~6 inner connections (parallel
// table copy), so the outer fan-out is held to 4 to stay under a
// typical 100-conn ceiling. Postgres SnapshotRestore is a single
// `CREATE DATABASE … TEMPLATE` statement; the bottleneck is disk
// throughput (block-copy of template files), so an outer wider than
// 8 just thrashes seeks without improving wall-clock — cap there.
var fanOutLimits = map[string]int{
	"mysql":      4,
	"mariadb":    4,
	"tidb":       4,
	"postgres":   8,
	"postgresql": 8,
}

// innerConnsPerRestore models how many backend connections one
// SnapshotRestore holds open in parallel. The auto-tuner divides the
// server's available connection budget by this number to find a safe
// outer fan-out. MySQL's parallel-table-copy worker count is the
// dominant term; Postgres only needs the one CREATE DATABASE
// session. Unknown engines collapse to 1 (no inner parallelism
// assumed).
var innerConnsPerRestore = map[string]int{
	"mysql":      mysqlInnerPerRestore,
	"mariadb":    mysqlInnerPerRestore,
	"tidb":       mysqlInnerPerRestore,
	"postgres":   1,
	"postgresql": 1,
}

// mysqlInnerPerRestore mirrors mysqlCloneFanout in the mysql driver.
// Duplicated here as a constant so the prepare package doesn't drag
// in a driver import for one int.
const mysqlInnerPerRestore = 6

// autoTuneOuter computes a connection-budget-aware outer fanout when
// no operator override is provided. The formula keeps a 10% (or 5-
// connection, whichever is bigger) headroom for the rest of the
// system, then divides the remainder by the engine's per-restore
// connection cost.
//
// Returned value is also bounded by [2, fanOutLimits[engine] * 2] so
// a misconfigured `max_connections=10000` doesn't spawn thousands of
// restorer goroutines. Per-call `len(clones)` clamping happens at
// the use site.
func autoTuneOuter(engine string, maxConns int) int {
	inner := innerConnsPerRestore[engine]
	if inner < 1 {
		inner = 1
	}
	defaultCap := fanOutLimits[engine]
	if defaultCap < 2 {
		defaultCap = 4
	}
	if maxConns <= 0 {
		return defaultCap
	}
	reserved := maxConns / 10
	if reserved < 5 {
		reserved = 5
	}
	available := maxConns - reserved
	if available < inner {
		return 2
	}
	outer := available / inner
	if outer < 2 {
		outer = 2
	}
	// Ceiling is 2× the conservative default — beyond that we hit
	// engine-side serialization (Postgres template I/O, MySQL ROW
	// lock contention) and gain nothing.
	hardCap := defaultCap * 2
	if outer > hardCap {
		outer = hardCap
	}
	return outer
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
// Events emitted: `fanout_start` (info) at entry, per-clone
// `clone_restore_done` (debug) / `clone_restore_fail` (warn) inside
// each restore, and `fanout_done` (info / error on failure) at exit
// with the slowest-clone duration so the user can spot stragglers
// without trawling the per-clone debug stream.
//
// Returns the first error seen. errgroup's ctx cancellation
// propagates so peer restores abort instead of hammering a server
// that already refused the first one.
func fanOutClones(
	ctx context.Context,
	st *store.Store,
	repoID, worktreeID int64,
	restore cloneRestorer,
	template string,
	clones []string,
	engine string,
	override uint32,
	maxConns int,
) error {
	if len(clones) == 0 {
		return nil
	}
	autoTuned := false
	limit := 0
	if override > 0 {
		limit = int(override)
	} else if maxConns > 0 {
		limit = autoTuneOuter(engine, maxConns)
		autoTuned = true
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
	_ = autoTuned // surfaced via the fanout_start payload below

	startedMs := time.Now().UnixMilli()
	if st != nil {
		_ = st.WriteEvent(ctx, store.LevelInfo, "fanout_start",
			fmt.Sprintf("engine=%s template=%s clones=%d limit=%d", engine, template, len(clones), limit),
			repoID, worktreeID, "", 0, map[string]any{
				"engine":     engine,
				"template":   template,
				"clones":     len(clones),
				"limit":      limit,
				"auto_tuned": autoTuned,
				"max_conns":  maxConns,
			})
	}

	var (
		okCount   atomic.Uint32
		failCount atomic.Uint32
		slowestMs atomic.Int64
		slowestMu sync.Mutex
		slowestDB string
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for _, c := range clones {
		c := c
		g.Go(func() error {
			cloneStart := time.Now()
			err := restore(gctx, template, c)
			dur := time.Since(cloneStart).Milliseconds()
			if err != nil {
				failCount.Add(1)
				if st != nil {
					_ = st.WriteEvent(gctx, store.LevelWarn, "clone_restore_fail",
						fmt.Sprintf("engine=%s db=%s err=%v", engine, c, err),
						repoID, worktreeID, "", dur, map[string]any{
							"engine":   engine,
							"template": template,
							"db":       c,
							"error":    err.Error(),
						})
				}
				return fmt.Errorf("restore %s → %s: %w", template, c, err)
			}
			okCount.Add(1)
			if st != nil {
				_ = st.WriteEvent(gctx, store.LevelDebug, "clone_restore_done",
					fmt.Sprintf("engine=%s db=%s", engine, c),
					repoID, worktreeID, "", dur, map[string]any{
						"engine":   engine,
						"template": template,
						"db":       c,
					})
			}
			if cur := slowestMs.Load(); dur > cur {
				slowestMu.Lock()
				if dur > slowestMs.Load() {
					slowestMs.Store(dur)
					slowestDB = c
				}
				slowestMu.Unlock()
			}
			return nil
		})
	}
	gErr := g.Wait()

	totalMs := time.Now().UnixMilli() - startedMs
	if st != nil {
		level := store.LevelInfo
		if failCount.Load() > 0 {
			level = store.LevelError
		}
		slowestMu.Lock()
		slowDB := slowestDB
		slowestMu.Unlock()
		_ = st.WriteEvent(ctx, level, "fanout_done",
			fmt.Sprintf("engine=%s ok=%d fail=%d slowest=%dms",
				engine, okCount.Load(), failCount.Load(), slowestMs.Load()),
			repoID, worktreeID, "", totalMs, map[string]any{
				"engine":     engine,
				"template":   template,
				"ok":         okCount.Load(),
				"fail":       failCount.Load(),
				"slowest_ms": slowestMs.Load(),
				"slowest_db": slowDB,
			})
	}
	return gErr
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
	return RunFiltered(ctx, cfg, worktreePath, sl, st, repoID, worktreeID, inheritedEnv, RunOptions{})
}

// RunOptions tunes RunFiltered. Zero value runs every database with
// normal cache-hit semantics — equivalent to the pre-refactor `Run`.
type RunOptions struct {
	// OnlyDBIndex restricts the run to a single database when >= 0.
	// Used by the watcher path so an edit under `databases[i].watch`
	// only re-prepares that one database. Default 0 means "no
	// filter" — callers must set OnlyDBIndex = -1 to *explicitly*
	// skip filtering, OR leave it at the zero value via Run().
	OnlyDBIndex int
	// FilterDBs is true when OnlyDBIndex should be honoured.
	FilterDBs bool
}

// RunFiltered prepares the configured databases, optionally
// restricted to one DB and/or forced to rebuild past the cache.
// Used by the daemon's watcher dispatch path so `on: rebuild` on a
// `databases[i].watch` glob actually forces a fresh template build
// for `databases[i]`, not just a cache-hit restore.
func RunFiltered(
	ctx context.Context,
	cfg *config.Config,
	worktreePath string,
	sl slug.Slug,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
	opts RunOptions,
) ([]Outcome, error) {
	if runid.From(ctx) == "" {
		ctx = runid.With(ctx, runid.New())
	}
	tplCtx := template.FromSlug(sl)

	results := make([]Outcome, len(cfg.Databases))
	hasResult := make([]bool, len(cfg.Databases))
	g, gctx := errgroup.WithContext(ctx)
	for i, d := range cfg.Databases {
		i, d := i, d
		if opts.FilterDBs && opts.OnlyDBIndex != i {
			continue
		}
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
				o, err = prepareMongo(gctx, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			case "redis":
				o, err = prepareRedis(gctx, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			case "elasticsearch", "opensearch":
				o, err = prepareES(gctx, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
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
	started := time.Now()
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
	maxConns, _ := drv.MaxConnections(ctx)

	key := computeSnapshotKey(ctx, st, d, worktreePath, sourceDB, version)
	templateName := key.TemplateName()

	// Cache lookup: if SQLite knows a snapshot row for this
	// fingerprint AND the template DB still exists in MySQL, skip
	// the cold build and just clone the template into paratest DBs.
	// Inputs feed the fingerprint, so any user-meaningful change
	// invalidates the cache naturally — no force-rebuild knob.
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
			// Cache-hit skips the cold-build path that populates the
			// source DB via dump+migrate. Restore source from the
			// template so non-parallel runs (single `php artisan test`,
			// dev shells, tinker — anything that reads DB_DATABASE
			// without the `_test_N` suffix paratest appends) have a
			// populated DB at the user-facing name_template.
			if err := drv.SnapshotRestore(ctx, rec.TemplateName, sourceDB); err != nil {
				return Outcome{}, fmt.Errorf("restore source %s from template %s: %w", sourceDB, rec.TemplateName, err)
			}
			if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, rec.TemplateName, clones, d.Engine, d.Fanout, maxConns); err != nil {
				return Outcome{}, err
			}
			_ = st.TouchSnapshot(ctx, key.Fingerprint())
			ms := time.Since(started).Milliseconds()
			_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
				fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
				repoID, worktreeID, "", 0, map[string]string{
					"source_db":   sourceDB,
					"template":    rec.TemplateName,
					"clones":      fmt.Sprintf("%d", len(clones)),
					"fingerprint": key.Fingerprint(),
					"cache_hit":   "true",
					"duration_ms": fmt.Sprintf("%d", ms),
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
	if dp, ok, err := dumpReady(d.Dump, worktreePath); err != nil {
		return Outcome{}, err
	} else if ok {
		stepStart := time.Now()
		if _, err := dumpload.LoadMySQL(ctx, drv.DB, sourceDB, dp); err != nil {
			return Outcome{}, fmt.Errorf("load dump %s: %w", dp, err)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "dump-load", stepStart)
	}
	if d.Migrate != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromMigrate(*d.Migrate), worktreePath, sourceDB, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("migrate source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("migrate source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "migrate", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromSeed(*d.Seed), worktreePath, sourceDB, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("seed source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("seed source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "seed", stepStart)
	}

	// Build the template snapshot, then clone it into paratest DBs.
	snapStart := time.Now()
	if err := drv.SnapshotCreate(ctx, sourceDB, templateName); err != nil {
		return Outcome{}, fmt.Errorf("snapshot create %s → %s: %w", sourceDB, templateName, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "snapshot-create", snapStart)
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    key.Fingerprint(),
		Engine:         d.Engine,
		EngineVersion:  version,
		SourceDB:       sourceDB,
		TemplateName:   templateName,
		MigrationsHash: key.MigrationsHashHex,
		DumpHash:       key.DumpHashHex,
		LockfileHashes: key.LockfileHashes,
		RepoID:         repoID,
	})
	// Fire-and-forget LRU eviction for this repo. Uses a fresh
	// background context with a hard deadline so a stalled DROP
	// (e.g. lock contention) can't leak this goroutine forever.
	// Errors are logged inside EvictExcess.
	go func() {
		evictCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		snapshot.EvictExcess(evictCtx, cfg, st, repoID)
	}()

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, templateName, clones, d.Engine, d.Fanout, maxConns); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("cold_build clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"source_db":   sourceDB,
			"template":    templateName,
			"clones":      fmt.Sprintf("%d", len(clones)),
			"cache_hit":   "false",
			"duration_ms": fmt.Sprintf("%d", ms),
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
	started := time.Now()
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
	maxConns, _ := drv.MaxConnections(ctx)

	key := computeSnapshotKey(ctx, st, d, worktreePath, sourceDB, version)
	templateName := key.TemplateName()

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=postgres source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "postgres",
			"source_db":   sourceDB,
			"template":    templateName,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit? Fingerprint covers every declared input.
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
			// Cache-hit skips the cold-build path that populates the
			// source DB via dump+migrate. Restore source from the
			// template so non-parallel runs (single test command,
			// dev shells — anything that reads the user-facing
			// name_template without the per-worker suffix) have a
			// populated DB.
			if err := drv.SnapshotRestore(ctx, rec.TemplateName, sourceDB); err != nil {
				return Outcome{}, fmt.Errorf("restore source %s from template %s: %w", sourceDB, rec.TemplateName, err)
			}
			if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, rec.TemplateName, clones, d.Engine, d.Fanout, maxConns); err != nil {
				return Outcome{}, err
			}
			_ = st.TouchSnapshot(ctx, key.Fingerprint())
			ms := time.Since(started).Milliseconds()
			_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
				fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
				repoID, worktreeID, "", 0, map[string]string{
					"source_db":   sourceDB,
					"template":    rec.TemplateName,
					"clones":      fmt.Sprintf("%d", len(clones)),
					"fingerprint": key.Fingerprint(),
					"cache_hit":   "true",
					"duration_ms": fmt.Sprintf("%d", ms),
				})
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
	if dp, ok, err := dumpReady(d.Dump, worktreePath); err != nil {
		return Outcome{}, err
	} else if ok {
		stepStart := time.Now()
		// Postgres has no USE, so dumpload needs a connection scoped
		// to the target DB rather than the server-level pool.
		scoped, err := drv.OpenScoped(ctx, sourceDB)
		if err != nil {
			return Outcome{}, err
		}
		if _, err := dumpload.LoadPostgres(ctx, scoped, sourceDB, dp); err != nil {
			scoped.Close()
			return Outcome{}, fmt.Errorf("load dump %s: %w", dp, err)
		}
		scoped.Close()
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "dump-load", stepStart)
	}
	if d.Migrate != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromMigrate(*d.Migrate), worktreePath, sourceDB, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("migrate source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("migrate source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "migrate", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromSeed(*d.Seed), worktreePath, sourceDB, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("seed source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("seed source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "seed", stepStart)
	}
	snapStart := time.Now()
	if err := drv.SnapshotCreate(ctx, sourceDB, templateName); err != nil {
		return Outcome{}, fmt.Errorf("snapshot create %s → %s: %w", sourceDB, templateName, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "snapshot-create", snapStart)
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint: key.Fingerprint(), Engine: d.Engine, EngineVersion: version,
		SourceDB: sourceDB, TemplateName: templateName,
		MigrationsHash: key.MigrationsHashHex, DumpHash: key.DumpHashHex, LockfileHashes: key.LockfileHashes,
		RepoID: repoID,
	})
	go snapshot.EvictExcess(context.Background(), cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, templateName, clones, d.Engine, d.Fanout, maxConns); err != nil {
		return Outcome{}, err
	}
	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("cold_build clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "postgres",
			"source_db":   sourceDB,
			"template":    templateName,
			"clones":      fmt.Sprintf("%d", len(clones)),
			"cache_hit":   "false",
			"duration_ms": fmt.Sprintf("%d", ms),
		})
	return Outcome{
		Engine: d.Engine, SourceDB: sourceDB, TemplateName: templateName,
		Fingerprint: key.Fingerprint(), CacheHit: false, Clones: clones,
	}, nil
}

// prepareMongo readies the per-worktree MongoDB database. Cold-build
// order matches the SQL engines: drop, optionally `mongorestore` a
// dump archive, run the seed step, snapshot the per-collection
// template, fan clones out. Cache-hit short-circuits straight to
// the per-collection $out clone.
func prepareMongo(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	started := time.Now()
	if cfg.Connections.Mongodb == nil {
		return Outcome{}, fmt.Errorf("connections.mongodb not configured")
	}
	sourceDB, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		return Outcome{}, err
	}
	defer drv.Close(ctx)

	version, _ := drv.EngineVersion(ctx)
	key := computeSnapshotKey(ctx, st, d, worktreePath, sourceDB, version)
	templateName := key.TemplateName()

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=mongodb source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "mongodb",
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
					"engine":      "mongodb",
					"source_db":   sourceDB,
					"template":    rec.TemplateName,
					"fingerprint": key.Fingerprint(),
				})
			clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
			if err != nil {
				return Outcome{}, err
			}
			if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, rec.TemplateName, clones, d.Engine, d.Fanout, 0); err != nil {
				return Outcome{}, err
			}
			_ = st.TouchSnapshot(ctx, key.Fingerprint())
			ms := time.Since(started).Milliseconds()
			_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
				fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":      "mongodb",
					"source_db":   sourceDB,
					"template":    rec.TemplateName,
					"clones":      fmt.Sprintf("%d", len(clones)),
					"fingerprint": key.Fingerprint(),
					"cache_hit":   "true",
					"duration_ms": fmt.Sprintf("%d", ms),
				})
			return Outcome{
				Engine: d.Engine, SourceDB: sourceDB,
				TemplateName: rec.TemplateName, Fingerprint: key.Fingerprint(),
				CacheHit: true, Clones: clones,
			}, nil
		}
		_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	}

	// Cold build: drop source, optionally mongorestore a dump
	// archive, run the seed step, snapshot template, fan out
	// clones.
	if _, err := drv.DropMatching(ctx, sourceDB); err != nil {
		return Outcome{}, fmt.Errorf("mongo drop %s: %w", sourceDB, err)
	}
	if dp, ok, err := dumpReady(d.Dump, worktreePath); err != nil {
		return Outcome{}, err
	} else if ok {
		stepStart := time.Now()
		if err := dbmongo.Restore(ctx, cfg.Connections.Mongodb.URI, sourceDB, d.Dump.SourceDB, dp); err != nil {
			return Outcome{}, fmt.Errorf("mongo restore %s: %w", dp, err)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "dump-load", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromSeed(*d.Seed), worktreePath, sourceDB, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("seed source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("seed source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "seed", stepStart)
	}
	snapStart := time.Now()
	if err := drv.SnapshotCreate(ctx, sourceDB, templateName); err != nil {
		return Outcome{}, fmt.Errorf("mongo snapshot create %s → %s: %w", sourceDB, templateName, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "snapshot-create", snapStart)
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint: key.Fingerprint(), Engine: d.Engine, EngineVersion: version,
		SourceDB: sourceDB, TemplateName: templateName,
		MigrationsHash: key.MigrationsHashHex, DumpHash: key.DumpHashHex, LockfileHashes: key.LockfileHashes,
		RepoID: repoID,
	})
	go snapshot.EvictExcess(context.Background(), cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, templateName, clones, d.Engine, d.Fanout, 0); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("cold_build clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "mongodb",
			"source_db":   sourceDB,
			"template":    templateName,
			"clones":      fmt.Sprintf("%d", len(clones)),
			"cache_hit":   "false",
			"duration_ms": fmt.Sprintf("%d", ms),
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

// prepareRedis brings up a worktree's Redis namespace using the
// same cache-hit / cold-build / parallel-fanout flow as every other
// engine. Two isolation modes:
//
//   - `namespaces.prefix_template` (preferred) — every key lives in
//     DB 0 under a per-worktree prefix. No 16-DB limit, no FLUSHDB
//     blast radius, works on cluster mode. Template caching via
//     SCAN + server-side COPY with prefix rewrite. Fanout produces
//     N independent prefixes from `test_clones.name_template`.
//   - `namespaces.db_index_template` (legacy) — per-worktree
//     logical DB (0-15). FLUSHDB clears the index; no template
//     caching since the 16-slot space can't hold both. Kept for
//     existing configs.
//
// At least one of the two templates must be set; if both are set,
// prefix wins. Cluster-mode Redis or any setup with >15 worktrees
// should use prefix mode.
func prepareRedis(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	if cfg.Connections.Redis == nil {
		return Outcome{}, fmt.Errorf("connections.redis not configured")
	}
	if d.KeyPrefix == "" {
		return Outcome{}, fmt.Errorf("redis: key_prefix required")
	}
	drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
	if err != nil {
		return Outcome{}, err
	}
	defer drv.Close()

	return prepareRedisPrefix(ctx, cfg, d, drv, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
}

// prepareRedisPrefix is the modern path — prefix-isolated keys in
// DB 0, fingerprint-cached template, parallel fanout.
func prepareRedisPrefix(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	drv *dbredis.Driver,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	started := time.Now()
	sourcePrefix, err := template.Render(d.KeyPrefix, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render key_prefix: %w", err)
	}
	version, _ := drv.EngineVersion(ctx)
	key := computeSnapshotKey(ctx, st, d, worktreePath, sourcePrefix, version)
	templatePrefix := "_tm:" + key.Fingerprint()[:16] + ":"

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=redis source=%s template=%s", sourcePrefix, templatePrefix),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "redis",
			"source_db":   sourcePrefix,
			"template":    templatePrefix,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit?
	if rec, err := st.LookupSnapshot(ctx, key.Fingerprint()); err == nil && rec != nil {
		alive, _ := drv.PrefixExists(ctx, rec.TemplateName)
		if alive {
			_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_hit",
				fmt.Sprintf("template=%s", rec.TemplateName),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":      "redis",
					"source_db":   sourcePrefix,
					"template":    rec.TemplateName,
					"fingerprint": key.Fingerprint(),
				})
			clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
			if err != nil {
				return Outcome{}, err
			}
			// Restore template → source so the worktree app sees fresh data.
			if err := drv.SnapshotRestore(ctx, rec.TemplateName, sourcePrefix); err != nil {
				return Outcome{}, fmt.Errorf("redis restore %s → %s: %w", rec.TemplateName, sourcePrefix, err)
			}
			if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, rec.TemplateName, clones, d.Engine, d.Fanout, 0); err != nil {
				return Outcome{}, err
			}
			_ = st.TouchSnapshot(ctx, key.Fingerprint())
			ms := time.Since(started).Milliseconds()
			_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
				fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":      "redis",
					"source_db":   sourcePrefix,
					"template":    rec.TemplateName,
					"clones":      fmt.Sprintf("%d", len(clones)),
					"fingerprint": key.Fingerprint(),
					"cache_hit":   "true",
					"duration_ms": fmt.Sprintf("%d", ms),
				})
			return Outcome{
				Engine: d.Engine, SourceDB: sourcePrefix,
				TemplateName: rec.TemplateName, Fingerprint: key.Fingerprint(),
				CacheHit: true, Clones: clones,
			}, nil
		}
		_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	}

	// Cold build: drop source, run seed, snapshot template, fanout.
	if _, err := drv.DropPrefix(ctx, sourcePrefix); err != nil {
		return Outcome{}, fmt.Errorf("redis drop %s*: %w", sourcePrefix, err)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromSeed(*d.Seed), worktreePath, sourcePrefix, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("seed redis %s: %w", sourcePrefix, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("seed redis %s exit %d: %s", sourcePrefix, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "seed", stepStart)
	}
	snapStart := time.Now()
	if err := drv.SnapshotCreate(ctx, sourcePrefix, templatePrefix); err != nil {
		return Outcome{}, fmt.Errorf("redis snapshot create %s → %s: %w", sourcePrefix, templatePrefix, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "snapshot-create", snapStart)
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint: key.Fingerprint(), Engine: d.Engine, EngineVersion: version,
		SourceDB: sourcePrefix, TemplateName: templatePrefix,
		MigrationsHash: key.MigrationsHashHex, DumpHash: key.DumpHashHex, LockfileHashes: key.LockfileHashes,
		RepoID: repoID,
	})
	go snapshot.EvictExcess(context.Background(), cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, templatePrefix, clones, d.Engine, d.Fanout, 0); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("cold_build clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "redis",
			"source_db":   sourcePrefix,
			"template":    templatePrefix,
			"clones":      fmt.Sprintf("%d", len(clones)),
			"cache_hit":   "false",
			"duration_ms": fmt.Sprintf("%d", ms),
		})
	return Outcome{
		Engine:       d.Engine,
		SourceDB:     sourcePrefix,
		TemplateName: templatePrefix,
		Fingerprint:  key.Fingerprint(),
		CacheHit:     false,
		Clones:       clones,
	}, nil
}

// prepareES brings up a worktree's Elasticsearch / OpenSearch
// indices using the same cache-hit / cold-build flow as MySQL +
// Postgres. The "namespace" for ES is an index-name prefix
// (multiple indices share it); template indices live under a
// fingerprint-keyed prefix so two worktrees with identical inputs
// share the cached set.
//
// On cache hit: clone every `<template-prefix><rest>` →
// `<source-prefix><rest>` via the native `_clone` API (server-side
// file-level copy).
//
// On cache miss: drop the source prefix, run the user's seed step
// (which populates the indices), then `SnapshotCreate` to copy
// `<source-prefix>*` into `<template-prefix>*`, then fan out clones
// into per-worker prefixes.
func prepareES(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	started := time.Now()
	if cfg.Connections.Elasticsearch == nil {
		return Outcome{}, fmt.Errorf("connections.elasticsearch not configured")
	}
	if d.KeyPrefix == "" {
		return Outcome{}, fmt.Errorf("elasticsearch: missing key_prefix")
	}
	sourcePrefix, err := template.Render(d.KeyPrefix, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render key_prefix: %w", err)
	}
	drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
	if err != nil {
		return Outcome{}, err
	}

	version, _ := drv.EngineVersion(ctx)
	key := computeSnapshotKey(ctx, st, d, worktreePath, sourcePrefix, version)
	// ES forbids index names starting with `_`, so we use a
	// dedicated prefix (tm_<fingerprint>_) for the template indices.
	templatePrefix := key.IndexPrefix() + "_"

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=elasticsearch source=%s template=%s", sourcePrefix, templatePrefix),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "elasticsearch",
			"source_db":   sourcePrefix,
			"template":    templatePrefix,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit? Verify by listing indices under the template prefix.
	if rec, err := st.LookupSnapshot(ctx, key.Fingerprint()); err == nil && rec != nil {
		alive, _ := drv.ListMatching(ctx, rec.TemplateName)
		if len(alive) > 0 {
			_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_hit",
				fmt.Sprintf("template=%s indices=%d", rec.TemplateName, len(alive)),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":      "elasticsearch",
					"source_db":   sourcePrefix,
					"template":    rec.TemplateName,
					"fingerprint": key.Fingerprint(),
				})
			clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
			if err != nil {
				return Outcome{}, err
			}
			if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, rec.TemplateName, clones, d.Engine, d.Fanout, 0); err != nil {
				return Outcome{}, err
			}
			_ = st.TouchSnapshot(ctx, key.Fingerprint())
			ms := time.Since(started).Milliseconds()
			_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
				fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":      "elasticsearch",
					"source_db":   sourcePrefix,
					"template":    rec.TemplateName,
					"clones":      fmt.Sprintf("%d", len(clones)),
					"fingerprint": key.Fingerprint(),
					"cache_hit":   "true",
					"duration_ms": fmt.Sprintf("%d", ms),
				})
			return Outcome{
				Engine: d.Engine, SourceDB: sourcePrefix,
				TemplateName: rec.TemplateName, Fingerprint: key.Fingerprint(),
				CacheHit: true, Clones: clones,
			}, nil
		}
		_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	}

	// Cold build: drop, optionally bulk-load a dump NDJSON, run
	// seed, snapshot, fanout.
	if _, err := drv.DropMatching(ctx, sourcePrefix); err != nil {
		return Outcome{}, fmt.Errorf("es drop %s*: %w", sourcePrefix, err)
	}
	if dp, ok, err := dumpReady(d.Dump, worktreePath); err != nil {
		return Outcome{}, err
	} else if ok {
		stepStart := time.Now()
		if err := drv.Restore(ctx, sourcePrefix, dp); err != nil {
			return Outcome{}, fmt.Errorf("es restore %s: %w", dp, err)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "dump-load", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromSeed(*d.Seed), worktreePath, sourcePrefix, inheritedEnv)
		if err != nil {
			return Outcome{}, fmt.Errorf("seed es %s: %w", sourcePrefix, err)
		}
		if out.ExitCode != 0 {
			return Outcome{}, fmt.Errorf("seed es %s exit %d: %s", sourcePrefix, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "seed", stepStart)
	}
	snapStart := time.Now()
	if err := drv.SnapshotCreate(ctx, sourcePrefix, templatePrefix); err != nil {
		return Outcome{}, fmt.Errorf("es snapshot create %s → %s: %w", sourcePrefix, templatePrefix, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "snapshot-create", snapStart)
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint: key.Fingerprint(), Engine: d.Engine, EngineVersion: version,
		SourceDB: sourcePrefix, TemplateName: templatePrefix,
		MigrationsHash: key.MigrationsHashHex, DumpHash: key.DumpHashHex, LockfileHashes: key.LockfileHashes,
		RepoID: repoID,
	})
	go snapshot.EvictExcess(context.Background(), cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, templatePrefix, clones, d.Engine, d.Fanout, 0); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("cold_build clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "elasticsearch",
			"source_db":   sourcePrefix,
			"template":    templatePrefix,
			"clones":      fmt.Sprintf("%d", len(clones)),
			"cache_hit":   "false",
			"duration_ms": fmt.Sprintf("%d", ms),
		})
	return Outcome{
		Engine:       d.Engine,
		SourceDB:     sourcePrefix,
		TemplateName: templatePrefix,
		Fingerprint:  key.Fingerprint(),
		CacheHit:     false,
		Clones:       clones,
	}, nil
}

// computeSnapshotKey hashes every input the snapshot fingerprint
// depends on (migration files, dump file, lockfile contents) and
// returns the canonical Key + the engine version it was built
// against. Pure helper — no engine I/O — so MySQL + Postgres share
// one path through the hashing primitives instead of duplicating
// the field assembly twice.
func computeSnapshotKey(
	ctx context.Context,
	st *store.Store,
	d config.DatabaseConfig,
	worktreePath, sourceDB, engineVersion string,
) snapshot.Key {
	// 1. Hash every input under databases[].inputs[]. Per-entry hash
	//    mode: `filename` for append-only directories (migrations),
	//    `checksum` (default) for everything else.
	inputHashes := map[string]string{}
	for _, in := range d.Inputs {
		mode := in.Hash
		if mode == "" {
			mode = "checksum"
		}
		switch mode {
		case "filename":
			spec := framework.Spec{
				Name:          fmt.Sprintf("input:%s", in.Glob),
				MigrationDirs: []string{staticGlobPrefix(in.Glob)},
				FileGlobs:     []string{filepath.Base(in.Glob)},
				HashMode:      framework.HashFilename,
			}
			if h, err := framework.MigrationsHashWithCache(ctx, frameworkHashCache{st}, worktreePath, spec); err == nil && h != "" {
				inputHashes[in.Glob] = h
			}
		default: // checksum
			matches, _ := filepath.Glob(filepath.Join(worktreePath, in.Glob))
			if len(matches) == 0 {
				continue
			}
			if hs, err := snapshot.LockfileHashesForWithCache(ctx, st, matches); err == nil {
				// Fold per-file hashes into one entry per glob — keyed
				// by the glob so the user can tell from the SQLite
				// row what changed.
				agg := ""
				for _, k := range sortedKeys(hs) {
					agg += k + ":" + hs[k] + "\n"
				}
				inputHashes[in.Glob] = agg
			}
		}
	}

	// 2. Hash the dump file (still a separate field — it's loaded
	//    into the engine, not just hashed).
	dumpHash := ""
	if d.Dump != nil {
		dp := filepath.Join(worktreePath, d.Dump.Path)
		hashes, _ := snapshot.LockfileHashesForWithCache(ctx, st, []string{dp})
		dumpHash = hashes[filepath.Base(dp)]
	}

	// 3. Hash the migrate + seed run-strings + env maps. Changing
	//    what the user told us to run is itself an input change.
	cmdHash := commandsHash(d)

	// Stash command hash + dump hash in lockfileHashes (the snapshot
	// Key already has a string-map field for ancillary inputs).
	if cmdHash != "" {
		inputHashes["__commands__"] = cmdHash
	}
	return snapshot.New(d.Engine, engineVersion, sourceDB, "", "", dumpHash, inputHashes)
}

// FingerprintReport is what InspectFingerprint surfaces to callers
// (notably the MCP inputs_fingerprint tool) — the current fingerprint
// for one configured database, the inputs that fed it, and whether
// a matching cached snapshot exists on disk.
type FingerprintReport struct {
	Engine            string                `json:"engine"`
	EngineVersion     string                `json:"engine_version"`
	SourceDB          string                `json:"source_db"`
	TemplateName      string                `json:"template_name"`
	Fingerprint       string                `json:"fingerprint"`
	DumpHash          string                `json:"dump_hash,omitempty"`
	InputHashes       map[string]string     `json:"input_hashes"`
	CommandsHash      string                `json:"commands_hash,omitempty"`
	CachedSnapshot    *store.SnapshotRecord `json:"cached_snapshot,omitempty"`
	CacheHitAvailable bool                  `json:"cache_hit_available"`
}

// InspectFingerprint computes the snapshot fingerprint for one
// configured database without doing any cold-build work — pure read
// path. Used by the MCP `inputs_fingerprint` tool so agents can
// answer "why did this prepare cold-build instead of hitting cache?"
// without rummaging through every input file by hand.
//
// engineVersion is supplied by the caller because probing it requires
// engine I/O, which is the MCP tool's responsibility (not this
// pure-CPU helper's). Pass an empty string if you only need a
// preview — the returned fingerprint won't match the cached one
// (engine version is mixed into the hash) but the per-input hashes
// will be accurate.
func InspectFingerprint(
	ctx context.Context,
	st *store.Store,
	d config.DatabaseConfig,
	worktreePath, sourceDB, engineVersion string,
) FingerprintReport {
	key := computeSnapshotKey(ctx, st, d, worktreePath, sourceDB, engineVersion)
	rep := FingerprintReport{
		Engine:        d.Engine,
		EngineVersion: engineVersion,
		SourceDB:      sourceDB,
		TemplateName:  key.TemplateName(),
		Fingerprint:   key.Fingerprint(),
		DumpHash:      key.DumpHashHex,
		InputHashes:   map[string]string{},
	}
	for k, v := range key.LockfileHashes {
		if k == "__commands__" {
			rep.CommandsHash = v
			continue
		}
		rep.InputHashes[k] = v
	}
	if st != nil {
		if rec, err := st.LookupSnapshot(ctx, key.Fingerprint()); err == nil && rec != nil {
			rep.CachedSnapshot = rec
			rep.CacheHitAvailable = true
		}
	}
	return rep
}

// staticGlobPrefix returns the longest leading directory of `glob`
// without glob meta-characters. Used to point the hash subsystem at
// the right tree to walk.
func staticGlobPrefix(glob string) string {
	out := ""
	for i, c := range glob {
		switch c {
		case '*', '?', '[':
			return out
		case '/':
			out = glob[:i]
		}
	}
	return glob
}

// commandsHash digests the user-declared migrate/seed run strings
// and env maps. If the user changes what gets run, the fingerprint
// flips — even when the input files themselves don't change.
func commandsHash(d config.DatabaseConfig) string {
	parts := []string{}
	if d.Migrate != nil {
		parts = append(parts, "migrate.run="+d.Migrate.Run)
		for _, k := range sortedKeys(d.Migrate.Env) {
			parts = append(parts, "migrate.env."+k+"="+d.Migrate.Env[k])
		}
	}
	if d.Seed != nil {
		parts = append(parts, "seed.run="+d.Seed.Run)
		for _, k := range sortedKeys(d.Seed.Env) {
			parts = append(parts, "seed.env."+k+"="+d.Seed.Env[k])
		}
	}
	if len(parts) == 0 {
		return ""
	}
	h := blake3.New(32, nil)
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sortedKeys returns the keys of `m` in sorted order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── Template-cache design notes ─────────────────────────────────
//
// MySQL and Postgres prepare paths follow a "EnsureTemplate then
// parallel FanoutClones" shape:
//
//   1. Hash every input that affects schema/data into a Key
//      (see computeSnapshotKey above).
//   2. Look up the Key in the SQLite snapshots table. If the engine
//      still has a matching template database alive, skip step 3.
//   3. Cold build: drop+recreate source, load dump, run migrations,
//      `SnapshotCreate(source, templateName)` to rename/copy the
//      populated state to a deterministic template-DB name keyed
//      off the fingerprint.
//   4. `fanOutClones` opens N parallel connections and restores
//      the template into each per-worker clone DB.
//
// Mongo / Redis / Elasticsearch don't yet have steps 2-3: each
// prepare just drops the per-worktree namespace and lets the app
// populate it on first write. To bring them up to MySQL/Postgres
// parity we need three primitives per engine:
//
//   • Dump(ctx, sourceNs, dumpPath) error
//     A way to serialise the populated state to disk under
//     treeman's snapshot cache dir, OR an in-engine "rename to
//     template name" primitive equivalent to MySQL's RENAME TABLE
//     dance + Postgres's CREATE DATABASE … TEMPLATE.
//   • EnsureTemplate(ctx, templateName, build func()) error
//     Build the template once; subsequent prepares find it ready.
//   • Clone(ctx, templateName, cloneName) error
//     Fast copy from template → per-worktree namespace. For ES this
//     is `POST /<template>/_clone/<clone>` (with a read-only flip
//     on the source). For Mongo it's mongodump|mongorestore or a
//     filesystem copy of the WiredTiger files. For Redis it's
//     `DEBUG RELOAD` against a per-worker DB index, or per-key
//     DUMP/RESTORE.
//
// All three engines also need a seed mechanism to populate the
// template the first time (postcreate hooks already serve this,
// but they fire per-worktree, not per-fingerprint). A future
// `databases[].seed:` field could capture the one-time bootstrap
// command — runs once into the template, never per worktree.
//

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
		if d.KeyPrefix == "" {
			return fmt.Errorf("redis: missing key_prefix")
		}
		drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
		if err != nil {
			return err
		}
		defer drv.Close()
		prefix, err := template.Render(d.KeyPrefix, tplCtx)
		if err != nil {
			return err
		}
		dropped, err := drv.DropPrefix(ctx, prefix)
		if err != nil {
			return err
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "db_drop",
			fmt.Sprintf("redis: %s (%d)", prefix, dropped),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": "redis", "slug": sl, "target": prefix, "count": dropped,
			})
		return nil
	case "elasticsearch", "opensearch":
		if cfg.Connections.Elasticsearch == nil {
			return fmt.Errorf("connections.elasticsearch not configured")
		}
		if d.KeyPrefix == "" {
			return fmt.Errorf("es: missing key_prefix")
		}
		drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if err != nil {
			return err
		}
		prefix, err := template.Render(d.KeyPrefix, tplCtx)
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
