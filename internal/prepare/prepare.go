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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"golang.org/x/sync/errgroup"
	"lukechampine.com/blake3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/dumpload"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/migrations/runner"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// runnerLogPath builds the disk path the runner should tee
// stdout+stderr into for a given prepare step. Lives alongside the
// hook logs under `<worktree>/.treeman-hooks/` so existing tooling and
// log-purge code picks it up automatically. Returns "" when the
// worktree path is empty so the runner skips disk teeing.
func runnerLogPath(worktreePath, engine, label, target string) string {
	if worktreePath == "" {
		return ""
	}
	safeTarget := strings.ReplaceAll(target, "/", "_")
	return filepath.Join(worktreePath, ".treeman-hooks",
		fmt.Sprintf("prepare-%s-%s-%s.log", engine, label, safeTarget))
}

// emitRunnerError records a runner failure (migrate/seed exit !=0)
// as a structured `prepare_error` event so the daemon's log query
// surfaces both stderr AND stdout tails. Earlier versions only logged
// stderr — Laravel/Symfony Console writes failure output to stdout, so
// the stderr-only event payload was empty and the failure reason was
// lost. Mirrors emitPhaseDone's shape: the run_id is auto-injected.
func emitRunnerError(ctx context.Context, st *store.Store, repoID, worktreeID int64,
	engine, sourceDB, label string, out runner.Outcome,
) {
	if st == nil {
		return
	}
	_ = st.WriteEvent(ctx, store.LevelError, "prepare_error",
		fmt.Sprintf("%s/%s phase=%s exit=%d", engine, sourceDB, label, out.ExitCode),
		repoID, worktreeID, label, 0, map[string]string{
			"engine":      engine,
			"source_db":   sourceDB,
			"exit_code":   strconv.Itoa(out.ExitCode),
			"stderr_tail": out.StderrTail,
			"stdout_tail": out.StdoutTail,
			"log_path":    out.LogPath,
		})
}

// emitPhaseDone writes a `prepare_phase` event tagged with the step
// name (dump-load, migrate, seed, snapshot-create) and the wall-clock
// duration measured from `stepStart`. Used by every per-engine
// prepare to give the user step-by-step visibility into where time
// is spent during a cold build. The run_id (set on ctx in the
// dispatch layer) is auto-injected into the payload by store.
func emitPhaseDone(ctx context.Context, st *store.Store, repoID, worktreeID int64,
	engine, sourceDB, phase string, stepStart time.Time,
) {
	if st == nil {
		return
	}
	durMs := time.Since(stepStart).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_phase",
		fmt.Sprintf("%s/%s phase=%s duration=%dms", engine, sourceDB, phase, durMs),
		repoID, worktreeID, phase, durMs, map[string]string{
			"engine":      engine,
			"source_db":   sourceDB,
			"duration_ms": strconv.FormatInt(durMs, 10),
		})
}

// dumpFile is one resolved dump entry ready for an engine loader.
// Path is the absolute, worktree-resolved file path; SourceDB carries
// the per-entry mongo namespace remap (empty for non-mongo engines).
type dumpFile struct {
	Path     string
	SourceDB string
}

// dumpsReady resolves a DumpList against worktreePath and returns the
// ordered list of dumps that should actually be loaded. Optional
// entries whose files are missing are silently skipped (matching the
// per-entry `optional: true` contract); required entries whose files
// are missing fail the whole list. An empty input list returns an
// empty (nil) slice — callers can branch on len(dumps) > 0.
func dumpsReady(dumps config.DumpList, worktreePath string) ([]dumpFile, error) {
	if len(dumps) == 0 {
		return nil, nil
	}
	out := make([]dumpFile, 0, len(dumps))
	for i, d := range dumps {
		p := filepath.Join(worktreePath, d.Path)
		if _, err := os.Stat(p); err != nil {
			if d.Optional {
				continue
			}
			return nil, fmt.Errorf("dump[%d] %s: %w", i, p, err)
		}
		out = append(out, dumpFile{Path: p, SourceDB: d.SourceDB})
	}
	return out, nil
}

// Cache-miss reason constants surfaced on `snapshot_cache_miss` events.
// `cacheMissNoRow` means LookupSnapshot returned nil (this fingerprint
// has never been built, or its row was GC'd). `cacheMissTemplateGone`
// means SQLite still has the row but the template DB / prefix is
// missing on the engine (someone dropped it out-of-band).
const (
	cacheMissNoRow        = "no_row"
	cacheMissTemplateGone = "template_gone"
)

// emitCloneStrategy records which SnapshotCreate path the engine
// driver actually took (currently physical / logical for mysql; the
// other engines have only one path so they don't emit). Pair with
// the prepare_phase `snapshot-create` event: that one says HOW LONG
// it took, this one says WHICH MECHANISM.
func emitCloneStrategy(ctx context.Context, st *store.Store, repoID, worktreeID int64,
	engine, sourceDB, templateName, strategy string,
) {
	if st == nil || strategy == "" {
		return
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_clone_strategy",
		fmt.Sprintf("engine=%s strategy=%s template=%s", engine, strategy, templateName),
		repoID, worktreeID, "snapshot-create", 0, map[string]string{
			"engine":    engine,
			"source_db": sourceDB,
			"template":  templateName,
			"strategy":  strategy,
		})
}

// emitCacheMiss writes a snapshot_cache_miss event explaining why a
// cache-hit lookup failed and what the engine is about to do instead
// (cold-build or incremental). Pair with the existing
// snapshot_cache_hit event: every prepare emits exactly one of the
// two for each declared database, so a user grepping the event stream
// can always tell which branch fired and why.
func emitCacheMiss(ctx context.Context, st *store.Store, repoID, worktreeID int64,
	engine, sourceDB, fingerprint, reason string,
) {
	if st == nil {
		return
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_miss",
		fmt.Sprintf("engine=%s source=%s reason=%s fingerprint=%s",
			engine, sourceDB, reason, fingerprint),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      engine,
			"source_db":   sourceDB,
			"fingerprint": fingerprint,
			"reason":      reason,
		})
}

// emitDumpLoadPhase emits a per-dump dump-load event tagged with the
// dump's path + index/total + the dispatcher `strategy` (docker-exec /
// native-cli / wire). Mirrors emitPhaseDone's shape with extra `path`,
// `index`, `total`, `strategy` fields. Use this in place of
// emitPhaseDone in dump-load loops so the user can attribute time to a
// specific file AND see which fast-path actually ran.
func emitDumpLoadPhase(ctx context.Context, st *store.Store, repoID, worktreeID int64,
	engine, sourceDB, path string, index, total int, stepStart time.Time, strategy string,
) {
	if st == nil {
		return
	}
	durMs := time.Since(stepStart).Milliseconds()
	base := filepath.Base(path)
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_phase",
		fmt.Sprintf("%s/%s phase=dump-load dump=%s (%d/%d) strategy=%s duration=%dms",
			engine, sourceDB, base, index+1, total, strategy, durMs),
		repoID, worktreeID, "dump-load", durMs, map[string]string{
			"engine":      engine,
			"source_db":   sourceDB,
			"duration_ms": strconv.FormatInt(durMs, 10),
			"path":        path,
			"index":       strconv.Itoa(index),
			"total":       strconv.Itoa(total),
			"strategy":    strategy,
		})
}

// Outcome — one row per database the orchestrator handled.
type Outcome struct {
	Engine       string
	SourceDB     string
	TemplateName string
	Fingerprint  string
	CacheHit     bool
	Clones       []string
	// Decision is set only for branch_scoped databases: how the active
	// namespace was filled this run — `seed:empty`, `seed:dump`,
	// `seed:parent`, `swap:resume`, `swap:parent`, `swap:branch-point`,
	// `adopt`, or `noop`. Empty for the template/clone path.
	Decision string
	// Skipped is true when the engine's connection block is absent
	// from the resolved config — declaring `databases: [{engine:
	// mysql, ...}]` without `connections.mysql` is now a no-op
	// (logged via `prepare_skipped`) instead of a wt_finalize error.
	// Lets repos like treeman's own checkout declare a sample
	// `.treeman.yaml` for schema-validation purposes without spawning
	// stale errors on every finalize.
	Skipped    bool
	SkipReason string
	// IncrementalBase is the fingerprint of the ancestor template this
	// run was built from when the incremental path was taken. Empty
	// otherwise. Lets callers (and tests) distinguish a full cold
	// build from an incremental build that ran migrate-only on top of
	// a cached ancestor.
	IncrementalBase string
}

// emitPrepareSkipped writes the `prepare_skipped` info event used by
// the unconfigured-engine short-circuit and returns the matching
// Outcome. Single point so the event payload stays consistent across
// every engine.
func emitPrepareSkipped(ctx context.Context, st *store.Store, repoID, worktreeID int64, eng, sourceDB, reason string) Outcome {
	if st != nil {
		_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_skipped",
			fmt.Sprintf("engine=%s reason=%s", eng, reason),
			repoID, worktreeID, "", 0, map[string]string{
				"engine":    eng,
				"source_db": sourceDB,
				"reason":    reason,
			})
	}
	return Outcome{Engine: eng, SourceDB: sourceDB, Skipped: true, SkipReason: reason}
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
var fanOutLimits = map[engine.Family]int{
	engine.FamilyMySQL:    4,
	engine.FamilyPostgres: 8,
}

// innerConnsPerRestore models how many backend connections one
// SnapshotRestore holds open in parallel. The auto-tuner divides the
// server's available connection budget by this number to find a safe
// outer fan-out. MySQL's parallel-table-copy worker count is the
// dominant term; Postgres only needs the one CREATE DATABASE
// session. Unknown engines collapse to 1 (no inner parallelism
// assumed).
var innerConnsPerRestore = map[engine.Family]int{
	engine.FamilyMySQL:    mysqlInnerPerRestore,
	engine.FamilyPostgres: 1,
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
func autoTuneOuter(fam engine.Family, maxConns int) int {
	inner := max(innerConnsPerRestore[fam], 1)
	defaultCap := fanOutLimits[fam]
	if defaultCap < 2 {
		defaultCap = 4
	}
	if maxConns <= 0 {
		return defaultCap
	}
	reserved := max(maxConns/10, 5)
	available := maxConns - reserved
	if available < inner {
		return 2
	}
	outer := max(available/inner, 2)
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
	limit, autoTuned := fanOutLimit(override, maxConns, len(clones), engine)

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

// fanOutLimit picks the concurrency cap for a fan-out restore. Explicit
// override wins; otherwise auto-tune from the pool's max connections;
// otherwise a per-engine default (falling back to GOMAXPROCS). The
// result is clamped to [2, numClones]. autoTuned reports whether the
// max-connections heuristic was used (surfaced in the fanout_start
// event payload).
func fanOutLimit(override uint32, maxConns, numClones int, eng string) (limit int, autoTuned bool) {
	fam, _ := engine.Canonical(eng)
	switch {
	case override > 0:
		limit = int(override)
	case maxConns > 0:
		limit = autoTuneOuter(fam, maxConns)
		autoTuned = true
	default:
		limit = fanOutLimits[fam]
		if limit == 0 {
			limit = runtime.GOMAXPROCS(0)
		}
	}
	if limit < 2 {
		limit = 2
	}
	if limit > numClones {
		limit = numClones
	}
	return limit, autoTuned
}

// cacheHitRestoreAndFanout repopulates the user-facing source namespace
// AND every test clone from `template` in a SINGLE parallel fan-out.
//
// Folding the source restore into the fan-out set is the win: the bare
// name_template DB (read by non-parallel test runs, dev shells, tinker —
// anything that connects without the per-worker `_test_N` suffix) is
// rebuilt CONCURRENTLY with the clones instead of serially in front of
// them. That removes one full template-copy of latency from every cache
// hit — and post-v5 (content-only fingerprint) a cache hit is the common
// path for every fresh worktree of a known repo/branch.
//
// On any failure the whole cache hit is abandoned: the helper emits
// snapshot_cache_fallback, drops the now-suspect snapshot row, and
// returns the error so the engine falls through to a cold build. This
// merges the previously-separate restore-fail and fanout-fail fallbacks
// into one path with identical semantics.
func cacheHitRestoreAndFanout(
	ctx context.Context,
	st *store.Store,
	repoID, worktreeID int64,
	restore cloneRestorer,
	d config.DatabaseConfig,
	template, source string,
	clones []string,
	maxConns int,
	fingerprint string,
) error {
	targets := make([]string, 0, len(clones)+1)
	targets = append(targets, source)
	for _, c := range clones {
		if c != source {
			targets = append(targets, c)
		}
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, restore, template, targets, d.Engine, d.Fanout, maxConns); err != nil {
		if st != nil {
			_ = st.WriteEvent(ctx, store.LevelWarn, "snapshot_cache_fallback",
				fmt.Sprintf("restore/fanout failed, falling back to cold build: %v", err),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":      d.Engine,
					"template":    template,
					"fingerprint": fingerprint,
					"error":       err.Error(),
				})
			_ = st.DeleteSnapshot(ctx, fingerprint)
		}
		return err
	}
	return nil
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
		if opts.FilterDBs && opts.OnlyDBIndex != i {
			continue
		}
		g.Go(func() error {
			var (
				o   Outcome
				err error
			)
			fam, ok := engine.Canonical(d.Engine)
			if !ok {
				// Engine not recognised. Surface via event so the
				// user notices, but don't fail the whole prepare run.
				_ = st.WriteEvent(gctx, store.LevelWarn, "prepare_unsupported_engine",
					fmt.Sprintf("engine=%s not recognised", d.Engine),
					repoID, worktreeID, "", 0, nil)
				return nil
			}
			switch fam {
			case engine.FamilyMySQL:
				o, err = prepareMySQL(gctx, cfg, d, i, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			case engine.FamilyPostgres:
				o, err = preparePostgres(gctx, cfg, d, i, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			case engine.FamilyMongo:
				o, err = prepareMongo(gctx, cfg, d, i, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			case engine.FamilyRedis:
				o, err = prepareRedis(gctx, cfg, d, i, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			case engine.FamilyES:
				o, err = prepareES(gctx, cfg, d, i, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
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

//nolint:funlen // mirrors the linear cache-hit / incremental / cold-build / fanout flow used by every engine; extracting helpers just spreads the same conditions across functions
func prepareMySQL(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	dbIdx int,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	started := time.Now()
	sourceDB, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}
	if cfg.Connections.Mysql == nil {
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, sourceDB,
			"connections.mysql not configured"), nil
	}

	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = drv.Close() }()

	version, _ := drv.EngineVersion(ctx)
	maxConns, _ := drv.MaxConnections(ctx)

	// branch_scoped databases bypass the fingerprint template cache
	// entirely — their content is per-branch live data swapped through
	// the active namespace, not a migrations-only template shared
	// across worktrees.
	if d.BranchScoped {
		return runBranchScoped(ctx, branchScopedArgs{
			cfg: cfg, d: d, dbIdx: dbIdx, tplCtx: tplCtx, worktreePath: worktreePath,
			st: st, repoID: repoID, worktreeID: worktreeID, inheritedEnv: inheritedEnv,
			eng: &branchEngine{drv: mysqlNS{drv}, scope: scopeName, engine: "mysql"},
			loadDump: func(ctx context.Context, active string, dump dumpFile) error {
				_, e := dumpload.LoadMySQL(ctx, drv.DB, cfg.Connections.Mysql, active, dump.Path)
				return e
			},
		})
	}

	key := computeSnapshotKey(ctx, st, d, worktreePath, version)
	templateName := key.TemplateName()

	// Pin the fingerprint for the lifetime of this prepare so the
	// automated GC sweeps (EvictExcess, SweepBy*) can't drop the
	// template we're about to use / about to create. Without the pin,
	// a sibling worktree's cold-build that finishes between our
	// LookupSnapshot and our SnapshotRestore/fanout can evict the
	// row + DROP DATABASE the template, leaving us with `Unknown
	// database '_tm_…'` mid-restore.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	// Cache lookup: if SQLite knows a snapshot row for this
	// fingerprint AND the template DB still exists in MySQL, skip
	// the cold build and just clone the template into paratest DBs.
	// Inputs feed the fingerprint, so any user-meaningful change
	// invalidates the cache naturally — no force-rebuild knob.
	out, done, err := mysqlCacheHit(ctx, drv, d, tplCtx, worktreePath, st, repoID, worktreeID, sourceDB, key, maxConns, started)
	if done || err != nil {
		return out, err
	}

	// Per-input vectors used for ancestor lookup AND persisted into
	// the new snapshot row so future preps can build incrementally
	// off this one.
	inputs := computeInputVectors(ctx, st, d, worktreePath)

	// Incremental path: try to find a content-PREFIX ancestor template
	// (same engine/version/dump/commands; every input vector a prefix
	// of this run's) and clone + migrate from there instead of a full
	// cold build. The migration framework's own ledger skips the
	// already-applied files so only the new ones run.
	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, maxConns, key, inputs,
		inheritedEnv, started, incrementalOps{
			exists:          drv.DatabaseExists,
			snapshotRestore: drv.SnapshotRestore,
			snapshotCreate: func(ctx context.Context, src, tmpl string) error {
				if cerr := drv.SnapshotCreate(ctx, src, tmpl); cerr != nil {
					return cerr
				}
				emitCloneStrategy(ctx, st, repoID, worktreeID, d.Engine, src, tmpl,
					string(drv.LastCloneStrategy()))
				return nil
			},
		})
	if done || err != nil {
		return out, err
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
	//
	// DropDatabase, NOT DropMatching: sourceDB under the main-wt
	// overlay (e.g. `kontainer_testing`) is a prefix of every linked
	// worktree's `<sourceDB>_<slug>` source + per-test clones, so a
	// prefix drop would wipe sibling worktrees. Stale per-test
	// clones from a prior cold-build get overwritten by exact name
	// in SnapshotRestore during fanout below.
	if err := mysqlColdBuildSteps(ctx, drv, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, sourceDB, inheritedEnv); err != nil {
		return Outcome{}, err
	}

	// Build the template snapshot, then clone it into paratest DBs.
	if err := mysqlSnapshotAndRecord(ctx, drv, cfg, d, st, repoID, worktreeID, sourceDB, templateName, version, key, inputs); err != nil {
		return Outcome{}, err
	}

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(
		ctx,
		st,
		repoID,
		worktreeID,
		drv.SnapshotRestore,
		templateName,
		clones,
		d.Engine,
		d.Fanout,
		maxConns,
	); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("cold_build clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"source_db":   sourceDB,
			"template":    templateName,
			"clones":      strconv.Itoa(len(clones)),
			"cache_hit":   "false",
			"duration_ms": strconv.FormatInt(ms, 10),
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

// mysqlCacheHit handles the MySQL fingerprint-cache fast path. It
// returns (out, true, nil) when the cached template was reused and the
// caller should return `out`; (Outcome{}, false, nil) when the cache
// missed or fell back (caller must cold-build); (Outcome{}, false, err)
// on a hard error. Extracted verbatim from prepareMySQL — the prior
// `goto mysqlColdBuild` jumps become `return Outcome{}, false, nil`.
func mysqlCacheHit(
	ctx context.Context,
	drv *dbmysql.Driver,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB string,
	key snapshot.Key,
	maxConns int,
	started time.Time,
) (Outcome, bool, error) {
	// A LookupSnapshot error is treated as a cache miss (fall through to
	// cold build), matching the original `err == nil && rec != nil` guard.
	rec, _ := st.LookupSnapshot(ctx, key.Fingerprint())
	if rec == nil {
		emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourceDB, key.Fingerprint(), cacheMissNoRow)
		return Outcome{}, false, nil
	}
	// Touch the row BEFORE DatabaseExists so a concurrent
	// EvictExcess sees this template as most-recently-used and
	// won't pick it as LRU while we're about to use it.
	_ = st.TouchSnapshot(ctx, key.Fingerprint())
	if exists, _ := drv.DatabaseExists(ctx, rec.TemplateName); exists {
		_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_hit",
			"template="+rec.TemplateName,
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      d.Engine,
				"source_db":   sourceDB,
				"template":    rec.TemplateName,
				"fingerprint": key.Fingerprint(),
			})
		clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
		if err != nil {
			return Outcome{}, false, err
		}
		// Cache-hit skips the cold-build path that populates the
		// source DB via dump+migrate. Restore source from the
		// template so non-parallel runs (single `php artisan test`,
		// dev shells, tinker — anything that reads DB_DATABASE
		// without the `_test_N` suffix paratest appends) have a
		// populated DB at the user-facing name_template. The source is
		// folded into the clone fan-out so it's repopulated in parallel
		// with the clones rather than serially before them.
		//
		// If the template disappears mid-flight (race with
		// EvictExcess between DatabaseExists and the restore/fanout),
		// fall through to cold build instead of failing wt_finalize.
		if err := cacheHitRestoreAndFanout(ctx, st, repoID, worktreeID, drv.SnapshotRestore,
			d, rec.TemplateName, sourceDB, clones, maxConns, key.Fingerprint()); err != nil {
			return Outcome{}, false, nil //nolint:nilerr // cache-miss fallback: the helper already logged + dropped the stale row; returning the (Outcome{}, false, nil) sentinel makes the engine cold-build
		}
		ms := time.Since(started).Milliseconds()
		_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
			fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
			repoID, worktreeID, "", 0, map[string]string{
				"source_db":   sourceDB,
				"template":    rec.TemplateName,
				"clones":      strconv.Itoa(len(clones)),
				"fingerprint": key.Fingerprint(),
				"cache_hit":   "true",
				"duration_ms": strconv.FormatInt(ms, 10),
			})
		return Outcome{
			Engine:       d.Engine,
			SourceDB:     sourceDB,
			TemplateName: rec.TemplateName,
			Fingerprint:  key.Fingerprint(),
			CacheHit:     true,
			Clones:       clones,
		}, true, nil
	}
	// Row stale (template was dropped externally). Wipe so the
	// cold-build path below overwrites it cleanly.
	emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourceDB, key.Fingerprint(), cacheMissTemplateGone)
	_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	return Outcome{}, false, nil
}

// mysqlColdBuildSteps runs the MySQL cold-build populate phase: drop +
// recreate the source DB, optionally load a dump, then run migrate and
// seed. Extracted verbatim from prepareMySQL's cold-build block.
func mysqlColdBuildSteps(
	ctx context.Context,
	drv *dbmysql.Driver,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB string,
	inheritedEnv map[string]string,
) error {
	// mysqlColdBuild: cold-build pre-drop site (see coldbuild_invariant_test).
	if err := drv.DropDatabase(ctx, sourceDB); err != nil {
		return err
	}
	emitColdBuildDrop(ctx, st, repoID, worktreeID, "mysql", sourceDB)
	if err := drv.EnsureDB(ctx, sourceDB); err != nil {
		return err
	}
	dumps, err := dumpsReady(d.Dump, worktreePath)
	if err != nil {
		return err
	}
	for i, dr := range dumps {
		stepStart := time.Now()
		strategy, err := dumpload.LoadMySQL(ctx, drv.DB, cfg.Connections.Mysql, sourceDB, dr.Path)
		if err != nil {
			return fmt.Errorf("load dump %s: %w", dr.Path, err)
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourceDB, dr.Path, i, len(dumps), stepStart, string(strategy))
	}
	if d.Migrate != nil {
		stepStart := time.Now()
		spec := runner.FromMigrate(*d.Migrate).WithLogPath(runnerLogPath(worktreePath, "mysql", "migrate", sourceDB))
		out, err := runner.Run(ctx, spec, worktreePath, sourceDB, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("migrate source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			emitRunnerError(ctx, st, repoID, worktreeID, "mysql", sourceDB, "migrate", out)
			return fmt.Errorf("%s", runner.FormatError("migrate source", sourceDB, out))
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "migrate", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		spec := runner.FromSeed(*d.Seed).WithLogPath(runnerLogPath(worktreePath, "mysql", "seed", sourceDB))
		out, err := runner.Run(ctx, spec, worktreePath, sourceDB, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("seed source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			emitRunnerError(ctx, st, repoID, worktreeID, "mysql", sourceDB, "seed", out)
			return fmt.Errorf("%s", runner.FormatError("seed source", sourceDB, out))
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "seed", stepStart)
	}
	return nil
}

// mysqlSnapshotAndRecord builds the template snapshot from the populated
// source, records the SQLite snapshot row, and fires the detached LRU
// eviction sweep. Extracted verbatim from prepareMySQL's snapshot/record
// block.
func mysqlSnapshotAndRecord(
	ctx context.Context,
	drv *dbmysql.Driver,
	cfg *config.Config,
	d config.DatabaseConfig,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB, templateName, version string,
	key snapshot.Key,
	inputs map[string]store.InputVector,
) error {
	snapStart := time.Now()
	if err := drv.SnapshotCreate(ctx, sourceDB, templateName); err != nil {
		return fmt.Errorf("snapshot create %s → %s: %w", sourceDB, templateName, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "snapshot-create", snapStart)
	emitCloneStrategy(ctx, st, repoID, worktreeID, d.Engine, sourceDB, templateName, string(drv.LastCloneStrategy()))
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    key.Fingerprint(),
		Engine:         d.Engine,
		EngineVersion:  version,
		SourceDB:       sourceDB,
		TemplateName:   templateName,
		MigrationsHash: key.MigrationsHashHex,
		DumpHash:       key.DumpHashHex,
		LockfileHashes: key.LockfileHashes,
		Inputs:         inputs,
		RepoID:         repoID,
	})
	// Fire-and-forget LRU eviction for this repo. Uses a fresh
	// background context with a hard deadline so a stalled DROP
	// (e.g. lock contention) can't leak this goroutine forever.
	// Errors are logged inside EvictExcess.
	spawnEvict(cfg, st, repoID)
	return nil
}

// spawnEvict fires LRU snapshot eviction for a repo in a detached
// goroutine. It uses a fresh background context with a hard 5-minute
// deadline so a stalled DROP (e.g. engine lock contention) can never
// leak the goroutine forever, and so the eviction outlives the prepare
// request's own context. Errors are logged inside EvictExcess.
func spawnEvict(cfg *config.Config, st *store.Store, repoID int64) {
	go func() {
		evictCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		snapshot.EvictExcess(evictCtx, cfg, st, repoID)
	}()
}

// incrementalOps bundles the engine-specific primitives the generic
// `tryIncrementalBuild` orchestrator needs. Each engine driver exposes
// these under different concrete types (DatabaseExists vs PrefixExists
// vs ListMatching), so wrapping them in `func` values lets one helper
// drive every engine.
type incrementalOps struct {
	// exists reports whether a namespace (DB name for name-scoped
	// engines, key/index prefix for prefix-scoped engines) is present
	// on the engine. Used to detect a ghost ancestor whose SQLite row
	// outlived its template DB.
	exists func(ctx context.Context, name string) (bool, error)
	// snapshotRestore copies a template namespace into a target. Same
	// signature as cloneRestorer; engines already expose this for the
	// cache-hit and fan-out paths so no new driver method is needed.
	snapshotRestore cloneRestorer
	// snapshotCreate copies a populated source into a NEW template
	// (mysql DDL+INSERT, postgres CREATE DATABASE TEMPLATE, mongo $out,
	// redis COPY, ES _clone). Wraps the per-driver SnapshotCreate.
	snapshotCreate func(ctx context.Context, source, template string) error
}

// tryIncrementalBuild looks for a content-prefix ancestor template and,
// when found, clones it into the source namespace and runs only `migrate`
// on top — letting the framework's own migrations ledger skip the
// already-applied files. Dump-load and seed are skipped (the ancestor
// template already includes them). On success it records a NEW snapshot
// row for this run's full fingerprint and fans out the clones, then
// returns (out, true, nil) so the caller short-circuits the cold-build
// path. (Outcome{}, false, nil) means "no incremental possible, do the
// full cold build."
//
// Generic across all five engines: takes engine-specific primitives
// (exists / snapshotRestore / snapshotCreate) in an `incrementalOps`
// struct so the orchestration is written once.
//
// Contract mirrors the cache-hit fallbacks: any error inside is
// converted into a fall-through so the cold-build path can still
// recover. The `prepare_incremental_start` /
// `prepare_incremental_fallback` events surface why the path was taken
// or abandoned.
//
//nolint:gocyclo,funlen // one self-contained orchestrator with explicit fall-through events — splitting just spreads the same branches across helpers without simplifying review
func tryIncrementalBuild(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	source, templateName, version string,
	maxConns int,
	key snapshot.Key,
	inputs map[string]store.InputVector,
	inheritedEnv map[string]string,
	started time.Time,
	ops incrementalOps,
) (Outcome, bool, error) {
	commandsHash := key.LockfileHashes[store.CommandsHashKey]
	anc, _ := st.FindAncestorSnapshot(ctx, repoID, d.Engine, version, key.DumpHashHex, commandsHash, inputs)
	if anc == nil {
		return Outcome{}, false, nil
	}
	// Pin the ancestor so a concurrent GC can't evict it while we're
	// in the middle of cloning + migrating from it.
	unpinAnc := snapshot.Pin(anc.Fingerprint)
	defer unpinAnc()

	exists, _ := ops.exists(ctx, anc.TemplateName)
	if !exists {
		// Ghost row: SQLite remembers the template but the engine
		// doesn't. Clear the row so future preps stop picking it and
		// fall through to a full cold build for this run.
		_ = st.DeleteSnapshot(ctx, anc.Fingerprint)
		return Outcome{}, false, nil
	}

	delta := vectorDelta(anc.Inputs, inputs)
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_incremental_start",
		fmt.Sprintf("engine=%s ancestor=%s files_added=%d", d.Engine, anc.TemplateName, delta),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":               d.Engine,
			"source_db":            source,
			"template":             templateName,
			"fingerprint":          key.Fingerprint(),
			"ancestor_template":    anc.TemplateName,
			"ancestor_fingerprint": anc.Fingerprint,
			"files_added":          strconv.Itoa(delta),
		})

	// Restore ancestor template → source. SnapshotRestore drops the
	// target first, so any stale source contents are cleared.
	restoreStart := time.Now()
	if err := ops.snapshotRestore(ctx, anc.TemplateName, source); err != nil {
		_ = st.WriteEvent(ctx, store.LevelWarn, "prepare_incremental_fallback",
			fmt.Sprintf("ancestor restore failed: %v", err),
			repoID, worktreeID, "", 0, map[string]string{
				"engine":               d.Engine,
				"ancestor_template":    anc.TemplateName,
				"ancestor_fingerprint": anc.Fingerprint,
				"error":                err.Error(),
			})
		return Outcome{}, false, nil
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, source, "incremental-restore", restoreStart)

	if d.Migrate != nil {
		migrateStart := time.Now()
		spec := runner.FromMigrate(*d.Migrate).WithLogPath(runnerLogPath(worktreePath, d.Engine, "migrate", source))
		out, err := runner.Run(ctx, spec, worktreePath, source, tplCtx, inheritedEnv)
		if err != nil {
			return Outcome{}, false, fmt.Errorf("incremental migrate %s: %w", source, err)
		}
		if out.ExitCode != 0 {
			emitRunnerError(ctx, st, repoID, worktreeID, d.Engine, source, "migrate", out)
			return Outcome{}, false, fmt.Errorf("%s", runner.FormatError("incremental migrate", source, out))
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, source, "migrate", migrateStart)
	}

	snapStart := time.Now()
	if err := ops.snapshotCreate(ctx, source, templateName); err != nil {
		return Outcome{}, false, fmt.Errorf("incremental snapshot create %s → %s: %w", source, templateName, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, source, "snapshot-create", snapStart)
	_ = st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    key.Fingerprint(),
		Engine:         d.Engine,
		EngineVersion:  version,
		SourceDB:       source,
		TemplateName:   templateName,
		MigrationsHash: key.MigrationsHashHex,
		DumpHash:       key.DumpHashHex,
		LockfileHashes: key.LockfileHashes,
		Inputs:         inputs,
		RepoID:         repoID,
	})
	spawnEvict(cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, false, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, ops.snapshotRestore, templateName,
		clones, d.Engine, d.Fanout, maxConns); err != nil {
		return Outcome{}, false, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("incremental clones=%d duration=%dms ancestor=%s files_added=%d",
			len(clones), ms, anc.TemplateName, delta),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":               d.Engine,
			"source_db":            source,
			"template":             templateName,
			"clones":               strconv.Itoa(len(clones)),
			"fingerprint":          key.Fingerprint(),
			"cache_hit":            "false",
			"incremental":          "true",
			"ancestor_fingerprint": anc.Fingerprint,
			"files_added":          strconv.Itoa(delta),
			"duration_ms":          strconv.FormatInt(ms, 10),
		})

	return Outcome{
		Engine:          d.Engine,
		SourceDB:        source,
		TemplateName:    templateName,
		Fingerprint:     key.Fingerprint(),
		CacheHit:        false,
		Clones:          clones,
		IncrementalBase: anc.Fingerprint,
	}, true, nil
}

// vectorDelta sums the per-input file count by which the current input
// vectors exceed the ancestor's. Used purely for observability — the
// raw "files_added" count tells the user how many migrations got
// applied on top of the ancestor template.
func vectorDelta(ancestor, current map[string]store.InputVector) int {
	n := 0
	for glob, curVec := range current {
		ancVec := ancestor[glob]
		if len(curVec) > len(ancVec) {
			n += len(curVec) - len(ancVec)
		}
	}
	return n
}

// preparePostgres mirrors prepareMySQL for the PostgreSQL engine.
// Uses `CREATE DATABASE … TEMPLATE` as the snapshot-and-fan-out
// primitive — fast because pg copies on-disk files instead of
// replaying SQL. Cache hit path identical to MySQL: SQLite
// fingerprint lookup + pg_database existence check.
//
//nolint:funlen // mirrors the linear cache-hit / incremental / cold-build / fanout flow used by every engine; extracting helpers just spreads the same conditions across functions
func preparePostgres(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	dbIdx int,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	started := time.Now()
	sourceDB, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}
	if cfg.Connections.Postgres == nil {
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, sourceDB,
			"connections.postgres not configured"), nil
	}

	drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = drv.Close() }()

	version, _ := drv.EngineVersion(ctx)
	maxConns, _ := drv.MaxConnections(ctx)

	if d.BranchScoped {
		return runBranchScoped(ctx, branchScopedArgs{
			cfg: cfg, d: d, dbIdx: dbIdx, tplCtx: tplCtx, worktreePath: worktreePath,
			st: st, repoID: repoID, worktreeID: worktreeID, inheritedEnv: inheritedEnv,
			eng: &branchEngine{drv: postgresNS{drv}, scope: scopeName, engine: "postgres"},
			loadDump: func(ctx context.Context, active string, dump dumpFile) error {
				scoped, e := drv.OpenScoped(ctx, active)
				if e != nil {
					return e
				}
				defer func() { _ = scoped.Close() }()
				_, e = dumpload.LoadPostgres(ctx, scoped, cfg.Connections.Postgres, active, dump.Path)
				return e
			},
		})
	}

	key := computeSnapshotKey(ctx, st, d, worktreePath, version)
	templateName := key.TemplateName()

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=postgres source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "postgres",
			"source_db":   sourceDB,
			"template":    templateName,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit? Fingerprint covers every declared input.
	out, done, err := postgresCacheHit(ctx, drv, d, tplCtx, worktreePath, st, repoID, worktreeID, sourceDB, key, maxConns, started)
	if done || err != nil {
		return out, err
	}

	// Per-input vectors used for ancestor lookup AND persisted into
	// the new snapshot row so future preps can build incrementally
	// off this one. Mirrors prepareMySQL.
	inputs := computeInputVectors(ctx, st, d, worktreePath)

	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, maxConns, key, inputs,
		inheritedEnv, started, incrementalOps{
			exists:          drv.DatabaseExists,
			snapshotRestore: drv.SnapshotRestore,
			snapshotCreate:  drv.SnapshotCreate,
		})
	if done || err != nil {
		return out, err
	}

	if err := postgresColdBuildSteps(ctx, drv, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, sourceDB, inheritedEnv); err != nil {
		return Outcome{}, err
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
		Inputs: inputs,
		RepoID: repoID,
	})
	spawnEvict(cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(
		ctx,
		st,
		repoID,
		worktreeID,
		drv.SnapshotRestore,
		templateName,
		clones,
		d.Engine,
		d.Fanout,
		maxConns,
	); err != nil {
		return Outcome{}, err
	}
	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
		fmt.Sprintf("cold_build clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "postgres",
			"source_db":   sourceDB,
			"template":    templateName,
			"clones":      strconv.Itoa(len(clones)),
			"cache_hit":   "false",
			"duration_ms": strconv.FormatInt(ms, 10),
		})
	return Outcome{
		Engine: d.Engine, SourceDB: sourceDB, TemplateName: templateName,
		Fingerprint: key.Fingerprint(), CacheHit: false, Clones: clones,
	}, nil
}

// postgresCacheHit handles the Postgres fingerprint-cache fast path.
// Contract mirrors mysqlCacheHit: (out, true, nil) → return out;
// (Outcome{}, false, nil) → fall through to cold build; (_, false, err)
// → hard error. Extracted verbatim from preparePostgres — the prior
// `goto pgColdBuild` jumps become `return Outcome{}, false, nil`.
func postgresCacheHit(
	ctx context.Context,
	drv *dbpostgres.Driver,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB string,
	key snapshot.Key,
	maxConns int,
	started time.Time,
) (Outcome, bool, error) {
	// A LookupSnapshot error is treated as a cache miss (fall through to
	// cold build), matching the original `err == nil && rec != nil` guard.
	rec, _ := st.LookupSnapshot(ctx, key.Fingerprint())
	if rec == nil {
		emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourceDB, key.Fingerprint(), cacheMissNoRow)
		return Outcome{}, false, nil
	}
	// Touch row early so concurrent EvictExcess sees this template
	// as most-recently-used and skips it as LRU candidate.
	_ = st.TouchSnapshot(ctx, key.Fingerprint())
	if exists, _ := drv.DatabaseExists(ctx, rec.TemplateName); exists {
		_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_hit",
			"template="+rec.TemplateName,
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      "postgres",
				"source_db":   sourceDB,
				"template":    rec.TemplateName,
				"fingerprint": key.Fingerprint(),
			})
		clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
		if err != nil {
			return Outcome{}, false, err
		}
		// Cache-hit skips the cold-build path that populates the
		// source DB via dump+migrate. Restore source from the
		// template so non-parallel runs (single test command,
		// dev shells — anything that reads the user-facing
		// name_template without the per-worker suffix) have a
		// populated DB. The source is folded into the clone fan-out so
		// it's repopulated in parallel with the clones.
		//
		// If the template disappears mid-flight (race with
		// EvictExcess between DatabaseExists and the restore/
		// fanout calls), fall through to cold build.
		if err := cacheHitRestoreAndFanout(ctx, st, repoID, worktreeID, drv.SnapshotRestore,
			d, rec.TemplateName, sourceDB, clones, maxConns, key.Fingerprint()); err != nil {
			return Outcome{}, false, nil //nolint:nilerr // cache-miss fallback: the helper already logged + dropped the stale row; returning the (Outcome{}, false, nil) sentinel makes the engine cold-build
		}
		ms := time.Since(started).Milliseconds()
		_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
			fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
			repoID, worktreeID, "", 0, map[string]string{
				"source_db":   sourceDB,
				"template":    rec.TemplateName,
				"clones":      strconv.Itoa(len(clones)),
				"fingerprint": key.Fingerprint(),
				"cache_hit":   "true",
				"duration_ms": strconv.FormatInt(ms, 10),
			})
		return Outcome{
			Engine: d.Engine, SourceDB: sourceDB,
			TemplateName: rec.TemplateName, Fingerprint: key.Fingerprint(),
			CacheHit: true, Clones: clones,
		}, true, nil
	}
	emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourceDB, key.Fingerprint(), cacheMissTemplateGone)
	_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	return Outcome{}, false, nil
}

// postgresColdBuildSteps runs the Postgres cold-build populate phase:
// drop + recreate the source DB, optionally load a dump (via a scoped
// connection), then run migrate and seed. Extracted verbatim from
// preparePostgres's cold-build block.
func postgresColdBuildSteps(
	ctx context.Context,
	drv *dbpostgres.Driver,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB string,
	inheritedEnv map[string]string,
) error {
	// pgColdBuild: cold-build pre-drop site (see coldbuild_invariant_test).
	// DropDatabase, NOT DropMatching — see the comment on the matching
	// mysql cold-build site.
	if err := drv.DropDatabase(ctx, sourceDB); err != nil {
		return err
	}
	emitColdBuildDrop(ctx, st, repoID, worktreeID, "postgres", sourceDB)
	if err := drv.EnsureDB(ctx, sourceDB); err != nil {
		return err
	}
	dumps, err := dumpsReady(d.Dump, worktreePath)
	if err != nil {
		return err
	}
	if len(dumps) > 0 {
		// Postgres has no USE, so dumpload needs a connection scoped
		// to the target DB rather than the server-level pool. Reuse
		// one scoped connection for the whole dump list — opening per
		// dump would burn a connect+SSL handshake on each iteration.
		scoped, err := drv.OpenScoped(ctx, sourceDB)
		if err != nil {
			return err
		}
		for i, dr := range dumps {
			stepStart := time.Now()
			strategy, lerr := dumpload.LoadPostgres(ctx, scoped, cfg.Connections.Postgres, sourceDB, dr.Path)
			if lerr != nil {
				_ = scoped.Close()
				return fmt.Errorf("load dump %s: %w", dr.Path, lerr)
			}
			emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourceDB, dr.Path, i, len(dumps), stepStart, string(strategy))
		}
		_ = scoped.Close()
	}
	if d.Migrate != nil {
		stepStart := time.Now()
		spec := runner.FromMigrate(*d.Migrate).WithLogPath(runnerLogPath(worktreePath, "postgres", "migrate", sourceDB))
		out, err := runner.Run(ctx, spec, worktreePath, sourceDB, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("migrate source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			emitRunnerError(ctx, st, repoID, worktreeID, "postgres", sourceDB, "migrate", out)
			return fmt.Errorf("%s", runner.FormatError("migrate source", sourceDB, out))
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "migrate", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		spec := runner.FromSeed(*d.Seed).WithLogPath(runnerLogPath(worktreePath, "postgres", "seed", sourceDB))
		out, err := runner.Run(ctx, spec, worktreePath, sourceDB, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("seed source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			emitRunnerError(ctx, st, repoID, worktreeID, "postgres", sourceDB, "seed", out)
			return fmt.Errorf("%s", runner.FormatError("seed source", sourceDB, out))
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "seed", stepStart)
	}
	return nil
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
	dbIdx int,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	started := time.Now()
	sourceDB, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}
	if cfg.Connections.Mongodb == nil {
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, sourceDB,
			"connections.mongodb not configured"), nil
	}
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = drv.Close(ctx) }()

	version, _ := drv.EngineVersion(ctx)

	if d.BranchScoped {
		return runBranchScoped(ctx, branchScopedArgs{
			cfg: cfg, d: d, dbIdx: dbIdx, tplCtx: tplCtx, worktreePath: worktreePath,
			st: st, repoID: repoID, worktreeID: worktreeID, inheritedEnv: inheritedEnv,
			eng: &branchEngine{drv: mongoNS{drv}, scope: scopeName, engine: "mongodb"},
			loadDump: func(ctx context.Context, active string, dump dumpFile) error {
				// Per-entry SourceDB: each dump in the list can carry its own
				// `source_db:` for `--nsFrom=<source_db>.* --nsTo=<active>.*`.
				_, e := dbmongo.Restore(ctx, cfg.Connections.Mongodb, active, dump.SourceDB, dump.Path)
				return e
			},
		})
	}

	key := computeSnapshotKey(ctx, st, d, worktreePath, version)
	templateName := key.TemplateName()

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=mongodb source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "mongodb",
			"source_db":   sourceDB,
			"template":    templateName,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit?
	out, done, err := mongoCacheHit(ctx, drv, d, tplCtx, worktreePath, st, repoID, worktreeID, sourceDB, key, started)
	if done || err != nil {
		return out, err
	}

	inputs := computeInputVectors(ctx, st, d, worktreePath)

	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, 0, key, inputs,
		inheritedEnv, started, incrementalOps{
			exists:          drv.DatabaseExists,
			snapshotRestore: drv.SnapshotRestore,
			snapshotCreate:  drv.SnapshotCreate,
		})
	if done || err != nil {
		return out, err
	}

	if err := mongoColdBuildSteps(ctx, drv, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, sourceDB, inheritedEnv); err != nil {
		return Outcome{}, err
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
		Inputs: inputs,
		RepoID: repoID,
	})
	spawnEvict(cfg, st, repoID)

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
			"clones":      strconv.Itoa(len(clones)),
			"cache_hit":   "false",
			"duration_ms": strconv.FormatInt(ms, 10),
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

// mongoCacheHit handles the MongoDB fingerprint-cache fast path.
// Contract mirrors mysqlCacheHit: (out, true, nil) → return out;
// (Outcome{}, false, nil) → fall through to cold build; (_, false, err)
// → hard error. Extracted verbatim from prepareMongo — the prior
// `goto mongoColdBuild` jump becomes `return Outcome{}, false, nil`.
func mongoCacheHit(
	ctx context.Context,
	drv *dbmongo.Driver,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB string,
	key snapshot.Key,
	started time.Time,
) (Outcome, bool, error) {
	// A LookupSnapshot error is treated as a cache miss (fall through to
	// cold build), matching the original `err == nil && rec != nil` guard.
	rec, _ := st.LookupSnapshot(ctx, key.Fingerprint())
	if rec == nil {
		emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourceDB, key.Fingerprint(), cacheMissNoRow)
		return Outcome{}, false, nil
	}
	// Touch row early so concurrent EvictExcess sees this template
	// as most-recently-used and skips it as LRU candidate.
	_ = st.TouchSnapshot(ctx, key.Fingerprint())
	if exists, _ := drv.DatabaseExists(ctx, rec.TemplateName); exists {
		_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_hit",
			"template="+rec.TemplateName,
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      "mongodb",
				"source_db":   sourceDB,
				"template":    rec.TemplateName,
				"fingerprint": key.Fingerprint(),
			})
		clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
		if err != nil {
			return Outcome{}, false, err
		}
		// Cache-hit skips the cold-build path that populates the source
		// DB via dump+migrate. Restore source from the template so
		// non-parallel runs (single test command, dev shells, anything
		// reading the bare name_template without a per-worker suffix)
		// see populated data — parity with mysql/postgres/redis. The
		// source is folded into the clone fan-out so it's repopulated
		// in parallel with the clones.
		//
		// If the template disappears mid-flight (race with
		// EvictExcess), fall through to cold build instead of
		// failing wt_finalize.
		if err := cacheHitRestoreAndFanout(ctx, st, repoID, worktreeID, drv.SnapshotRestore,
			d, rec.TemplateName, sourceDB, clones, 0, key.Fingerprint()); err != nil {
			return Outcome{}, false, nil //nolint:nilerr // cache-miss fallback: the helper already logged + dropped the stale row; returning the (Outcome{}, false, nil) sentinel makes the engine cold-build
		}
		ms := time.Since(started).Milliseconds()
		_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
			fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      "mongodb",
				"source_db":   sourceDB,
				"template":    rec.TemplateName,
				"clones":      strconv.Itoa(len(clones)),
				"fingerprint": key.Fingerprint(),
				"cache_hit":   "true",
				"duration_ms": strconv.FormatInt(ms, 10),
			})
		return Outcome{
			Engine: d.Engine, SourceDB: sourceDB,
			TemplateName: rec.TemplateName, Fingerprint: key.Fingerprint(),
			CacheHit: true, Clones: clones,
		}, true, nil
	}
	emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourceDB, key.Fingerprint(), cacheMissTemplateGone)
	_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	return Outcome{}, false, nil
}

// mongoColdBuildSteps runs the MongoDB cold-build populate phase: drop
// the source DB, optionally mongorestore a dump archive, then run
// migrate and seed. Extracted verbatim from prepareMongo's cold-build
// block.
func mongoColdBuildSteps(
	ctx context.Context,
	drv *dbmongo.Driver,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB string,
	inheritedEnv map[string]string,
) error {
	// mongoColdBuild: cold-build pre-drop site (see coldbuild_invariant_test).
	// Drop source, optionally mongorestore a dump archive, run the seed
	// step, snapshot template, fan out clones.
	//
	// DropDatabase, NOT DropMatching — see the comment on the
	// matching mysql cold-build site.
	if err := drv.DropDatabase(ctx, sourceDB); err != nil {
		return fmt.Errorf("mongo drop %s: %w", sourceDB, err)
	}
	emitColdBuildDrop(ctx, st, repoID, worktreeID, "mongodb", sourceDB)
	dumps, err := dumpsReady(d.Dump, worktreePath)
	if err != nil {
		return err
	}
	for i, dr := range dumps {
		stepStart := time.Now()
		strategy, rerr := dbmongo.Restore(ctx, cfg.Connections.Mongodb, sourceDB, dr.SourceDB, dr.Path)
		if rerr != nil {
			return fmt.Errorf("mongo restore %s: %w", dr.Path, rerr)
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourceDB, dr.Path, i, len(dumps), stepStart, string(strategy))
	}
	if d.Migrate != nil {
		stepStart := time.Now()
		spec := runner.FromMigrate(*d.Migrate).WithLogPath(runnerLogPath(worktreePath, "mongodb", "migrate", sourceDB))
		out, err := runner.Run(ctx, spec, worktreePath, sourceDB, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("migrate source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			emitRunnerError(ctx, st, repoID, worktreeID, "mongodb", sourceDB, "migrate", out)
			return fmt.Errorf("%s", runner.FormatError("migrate source", sourceDB, out))
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "migrate", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromSeed(*d.Seed), worktreePath, sourceDB, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("seed source %s: %w", sourceDB, err)
		}
		if out.ExitCode != 0 {
			return fmt.Errorf("seed source %s exit %d: %s", sourceDB, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "seed", stepStart)
	}
	return nil
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
	dbIdx int,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	if cfg.Connections.Redis == nil {
		// No sourceDB to render here — KeyPrefix is the source identifier
		// for redis, and rendering it before the skip is harmless but
		// noisy in the event payload, so pass the raw prefix template.
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, d.KeyPrefix,
			"connections.redis not configured"), nil
	}
	if d.KeyPrefix == "" {
		return Outcome{}, errors.New("redis: key_prefix required")
	}
	drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = drv.Close() }()

	if d.BranchScoped {
		return runBranchScoped(ctx, branchScopedArgs{
			cfg: cfg, d: d, dbIdx: dbIdx, tplCtx: tplCtx, worktreePath: worktreePath,
			st: st, repoID: repoID, worktreeID: worktreeID, inheritedEnv: inheritedEnv,
			eng: &branchEngine{drv: redisNS{drv}, scope: scopePrefix, engine: "redis"},
		})
	}

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
	key := computeSnapshotKey(ctx, st, d, worktreePath, version)
	templatePrefix := "_tm:" + key.Fingerprint()[:16] + ":"

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=redis source=%s template=%s", sourcePrefix, templatePrefix),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "redis",
			"source_db":   sourcePrefix,
			"template":    templatePrefix,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit?
	out, done, err := redisCacheHit(ctx, drv, d, tplCtx, worktreePath, st, repoID, worktreeID, sourcePrefix, key, started)
	if done || err != nil {
		return out, err
	}

	inputs := computeInputVectors(ctx, st, d, worktreePath)

	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourcePrefix, templatePrefix, version, 0, key, inputs,
		inheritedEnv, started, incrementalOps{
			exists:          drv.PrefixExists,
			snapshotRestore: drv.SnapshotRestore,
			snapshotCreate:  drv.SnapshotCreate,
		})
	if done || err != nil {
		return out, err
	}

	// Cold build: drop source, run seed, snapshot template, fanout.
	if err := redisColdBuildSteps(ctx, drv, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, sourcePrefix, inheritedEnv); err != nil {
		return Outcome{}, err
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
		Inputs: inputs,
		RepoID: repoID,
	})
	spawnEvict(cfg, st, repoID)

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
			"clones":      strconv.Itoa(len(clones)),
			"cache_hit":   "false",
			"duration_ms": strconv.FormatInt(ms, 10),
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

// redisCacheHit handles the Redis-prefix fingerprint-cache fast path.
// Contract mirrors mysqlCacheHit: (out, true, nil) → return out;
// (Outcome{}, false, nil) → fall through to cold build; (_, false, err)
// → hard error. Extracted verbatim from prepareRedisPrefix — the prior
// `goto redisColdBuild` jumps become `return Outcome{}, false, nil`.
func redisCacheHit(
	ctx context.Context,
	drv *dbredis.Driver,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourcePrefix string,
	key snapshot.Key,
	started time.Time,
) (Outcome, bool, error) {
	// A LookupSnapshot error is treated as a cache miss (fall through to
	// cold build), matching the original `err == nil && rec != nil` guard.
	rec, _ := st.LookupSnapshot(ctx, key.Fingerprint())
	if rec == nil {
		emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, key.Fingerprint(), cacheMissNoRow)
		return Outcome{}, false, nil
	}
	// Touch row early so concurrent EvictExcess sees this template
	// as most-recently-used and skips it as LRU candidate.
	_ = st.TouchSnapshot(ctx, key.Fingerprint())
	alive, _ := drv.PrefixExists(ctx, rec.TemplateName)
	if alive {
		_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_cache_hit",
			"template="+rec.TemplateName,
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      "redis",
				"source_db":   sourcePrefix,
				"template":    rec.TemplateName,
				"fingerprint": key.Fingerprint(),
			})
		clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
		if err != nil {
			return Outcome{}, false, err
		}
		// Restore template → source so the worktree app sees fresh
		// data, folded into the clone fan-out so it runs in parallel.
		// If the template disappears mid-flight (race with
		// EvictExcess), fall through to cold build.
		if err := cacheHitRestoreAndFanout(ctx, st, repoID, worktreeID, drv.SnapshotRestore,
			d, rec.TemplateName, sourcePrefix, clones, 0, key.Fingerprint()); err != nil {
			return Outcome{}, false, nil //nolint:nilerr // cache-miss fallback: the helper already logged + dropped the stale row; returning the (Outcome{}, false, nil) sentinel makes the engine cold-build
		}
		ms := time.Since(started).Milliseconds()
		_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
			fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      "redis",
				"source_db":   sourcePrefix,
				"template":    rec.TemplateName,
				"clones":      strconv.Itoa(len(clones)),
				"fingerprint": key.Fingerprint(),
				"cache_hit":   "true",
				"duration_ms": strconv.FormatInt(ms, 10),
			})
		return Outcome{
			Engine: d.Engine, SourceDB: sourcePrefix,
			TemplateName: rec.TemplateName, Fingerprint: key.Fingerprint(),
			CacheHit: true, Clones: clones,
		}, true, nil
	}
	emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, key.Fingerprint(), cacheMissTemplateGone)
	_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	return Outcome{}, false, nil
}

// redisColdBuildSteps runs the Redis-prefix cold-build populate phase:
// filtered drop of the source prefix, then migrate + seed. Extracted
// verbatim from prepareRedisPrefix's cold-build block.
func redisColdBuildSteps(
	ctx context.Context,
	drv *dbredis.Driver,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourcePrefix string,
	inheritedEnv map[string]string,
) error {
	// redisColdBuild: cold-build pre-drop site (see coldbuild_invariant_test).
	// DropPrefixFiltered, NOT DropPrefix: under the main-wt overlay
	// sourcePrefix can prefix every other worktree's
	// `<sourcePrefix><slug>_*` keys. Skip keys containing
	// `_<otherslug>` as a whole token so sibling worktrees'
	// branch-scoped keys survive.
	redisOthers := siblingSlugs(ctx, st, repoID, worktreeID)
	keepRedisKey := func(k string) bool { return !nameOwnedByOtherSlug(k, redisOthers) }
	droppedCount, err := drv.DropPrefixFiltered(ctx, sourcePrefix, keepRedisKey)
	if err != nil {
		return fmt.Errorf("redis drop %s*: %w", sourcePrefix, err)
	}
	emitColdBuildDrop(ctx, st, repoID, worktreeID, "redis",
		fmt.Sprintf("%s* (%d)", sourcePrefix, droppedCount))
	// Dump-load: redis dumps are RESP pipe-format files (the same shape
	// `redis-cli --pipe` accepts). The dispatcher picks docker exec /
	// native CLI / wire fallback the same way every other engine does.
	dumps, derr := dumpsReady(d.Dump, worktreePath)
	if derr != nil {
		return derr
	}
	for i, dr := range dumps {
		stepStart := time.Now()
		strategy, lerr := drv.Restore(ctx, cfg.Connections.Redis, sourcePrefix, dr.Path)
		if lerr != nil {
			return fmt.Errorf("load dump %s: %w", dr.Path, lerr)
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, dr.Path,
			i, len(dumps), stepStart, string(strategy))
	}
	if d.Migrate != nil {
		stepStart := time.Now()
		spec := runner.FromMigrate(*d.Migrate).WithLogPath(runnerLogPath(worktreePath, "redis", "migrate", sourcePrefix))
		out, err := runner.Run(ctx, spec, worktreePath, sourcePrefix, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("migrate redis %s: %w", sourcePrefix, err)
		}
		if out.ExitCode != 0 {
			emitRunnerError(ctx, st, repoID, worktreeID, "redis", sourcePrefix, "migrate", out)
			return fmt.Errorf("%s", runner.FormatError("migrate redis", sourcePrefix, out))
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "migrate", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromSeed(*d.Seed), worktreePath, sourcePrefix, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("seed redis %s: %w", sourcePrefix, err)
		}
		if out.ExitCode != 0 {
			return fmt.Errorf("seed redis %s exit %d: %s", sourcePrefix, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "seed", stepStart)
	}
	return nil
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
	dbIdx int,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	started := time.Now()
	if d.KeyPrefix == "" {
		return Outcome{}, errors.New("elasticsearch: missing key_prefix")
	}
	sourcePrefix, err := template.Render(d.KeyPrefix, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render key_prefix: %w", err)
	}
	if cfg.Connections.Elasticsearch == nil {
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix,
			"connections.elasticsearch not configured"), nil
	}
	drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
	if err != nil {
		return Outcome{}, err
	}

	if d.BranchScoped {
		return runBranchScoped(ctx, branchScopedArgs{
			cfg: cfg, d: d, dbIdx: dbIdx, tplCtx: tplCtx, worktreePath: worktreePath,
			st: st, repoID: repoID, worktreeID: worktreeID, inheritedEnv: inheritedEnv,
			eng: &branchEngine{drv: esNS{drv}, scope: scopePrefix, engine: "elasticsearch"},
			loadDump: func(ctx context.Context, active string, dump dumpFile) error {
				return drv.Restore(ctx, active, dump.Path)
			},
		})
	}

	version, _ := drv.EngineVersion(ctx)
	key := computeSnapshotKey(ctx, st, d, worktreePath, version)
	// ES forbids index names starting with `_`, so we use a
	// dedicated prefix (tm_<fingerprint>_) for the template indices.
	templatePrefix := key.IndexPrefix() + "_"

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_start",
		fmt.Sprintf("engine=elasticsearch source=%s template=%s", sourcePrefix, templatePrefix),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "elasticsearch",
			"source_db":   sourcePrefix,
			"template":    templatePrefix,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit? Verify by listing indices under the template prefix.
	out, done, err := esCacheHit(ctx, drv, d, tplCtx, worktreePath, st, repoID, worktreeID, sourcePrefix, key, started)
	if done || err != nil {
		return out, err
	}

	inputs := computeInputVectors(ctx, st, d, worktreePath)

	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourcePrefix, templatePrefix, version, 0, key, inputs,
		inheritedEnv, started, incrementalOps{
			exists: func(ctx context.Context, name string) (bool, error) {
				m, err := drv.ListMatching(ctx, name)
				return len(m) > 0, err
			},
			snapshotRestore: drv.SnapshotRestore,
			snapshotCreate:  drv.SnapshotCreate,
		})
	if done || err != nil {
		return out, err
	}

	if err := esColdBuildSteps(ctx, drv, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, sourcePrefix, inheritedEnv); err != nil {
		return Outcome{}, err
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
		Inputs: inputs,
		RepoID: repoID,
	})
	spawnEvict(cfg, st, repoID)

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
			"clones":      strconv.Itoa(len(clones)),
			"cache_hit":   "false",
			"duration_ms": strconv.FormatInt(ms, 10),
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

// esCacheHit handles the Elasticsearch fingerprint-cache fast path.
// Contract mirrors mysqlCacheHit: (out, true, nil) → return out;
// (Outcome{}, false, nil) → fall through to cold build; (_, false, err)
// → hard error. Extracted verbatim from prepareES — the prior
// `goto esColdBuild` jump becomes `return Outcome{}, false, nil`.
func esCacheHit(
	ctx context.Context,
	drv *dbes.Driver,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourcePrefix string,
	key snapshot.Key,
	started time.Time,
) (Outcome, bool, error) {
	// A LookupSnapshot error is treated as a cache miss (fall through to
	// cold build), matching the original `err == nil && rec != nil` guard.
	rec, _ := st.LookupSnapshot(ctx, key.Fingerprint())
	if rec == nil {
		emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, key.Fingerprint(), cacheMissNoRow)
		return Outcome{}, false, nil
	}
	// Touch row early so concurrent EvictExcess sees this template
	// as most-recently-used and skips it as LRU candidate.
	_ = st.TouchSnapshot(ctx, key.Fingerprint())
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
			return Outcome{}, false, err
		}
		// Cache-hit skips the cold-build path that populates the source
		// prefix via dump+migrate. Restore source from the template so
		// non-parallel runs (single test command, dev shells, anything
		// reading the bare key_prefix without a per-worker suffix) see
		// populated indices — parity with mysql/postgres/redis. The
		// source is folded into the clone fan-out so it's repopulated
		// in parallel with the clones.
		//
		// If the template disappears mid-flight (race with EvictExcess),
		// fall through to cold build.
		if err := cacheHitRestoreAndFanout(ctx, st, repoID, worktreeID, drv.SnapshotRestore,
			d, rec.TemplateName, sourcePrefix, clones, 0, key.Fingerprint()); err != nil {
			return Outcome{}, false, nil //nolint:nilerr // cache-miss fallback: the helper already logged + dropped the stale row; returning the (Outcome{}, false, nil) sentinel makes the engine cold-build
		}
		ms := time.Since(started).Milliseconds()
		_ = st.WriteEvent(ctx, store.LevelInfo, "prepare_done",
			fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      "elasticsearch",
				"source_db":   sourcePrefix,
				"template":    rec.TemplateName,
				"clones":      strconv.Itoa(len(clones)),
				"fingerprint": key.Fingerprint(),
				"cache_hit":   "true",
				"duration_ms": strconv.FormatInt(ms, 10),
			})
		return Outcome{
			Engine: d.Engine, SourceDB: sourcePrefix,
			TemplateName: rec.TemplateName, Fingerprint: key.Fingerprint(),
			CacheHit: true, Clones: clones,
		}, true, nil
	}
	emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, key.Fingerprint(), cacheMissTemplateGone)
	_ = st.DeleteSnapshot(ctx, key.Fingerprint())
	return Outcome{}, false, nil
}

// esColdBuildSteps runs the Elasticsearch cold-build populate phase:
// filtered drop of the source-prefix indices, optionally bulk-load a
// dump NDJSON, then run migrate and seed. Extracted verbatim from
// prepareES's cold-build block.
func esColdBuildSteps(
	ctx context.Context,
	drv *dbes.Driver,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourcePrefix string,
	inheritedEnv map[string]string,
) error {
	// esColdBuild: cold-build pre-drop site (see coldbuild_invariant_test).
	// Drop, optionally bulk-load a dump NDJSON, run seed, snapshot, fanout.
	//
	// DropMatchingFiltered, NOT DropMatching: under the main-wt
	// overlay sourcePrefix can prefix every other worktree's
	// `<sourcePrefix>_<slug>_*` indices. Skip names containing
	// `_<otherslug>` as a whole token so sibling worktrees'
	// branch-scoped indices survive.
	others := siblingSlugs(ctx, st, repoID, worktreeID)
	keep := func(n string) bool { return !nameOwnedByOtherSlug(n, others) }
	dropped, err := drv.DropMatchingFiltered(ctx, sourcePrefix, keep)
	if err != nil {
		return fmt.Errorf("es drop %s*: %w", sourcePrefix, err)
	}
	emitColdBuildDrop(ctx, st, repoID, worktreeID, "elasticsearch",
		fmt.Sprintf("%s* (%d)", sourcePrefix, len(dropped)))
	dumps, err := dumpsReady(d.Dump, worktreePath)
	if err != nil {
		return err
	}
	for i, dr := range dumps {
		stepStart := time.Now()
		strategy, rerr := drv.DispatchRestore(ctx, cfg.Connections.Elasticsearch, sourcePrefix, dr.Path)
		if rerr != nil {
			return fmt.Errorf("es restore %s: %w", dr.Path, rerr)
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, dr.Path,
			i, len(dumps), stepStart, string(strategy))
	}
	if d.Migrate != nil {
		stepStart := time.Now()
		spec := runner.FromMigrate(*d.Migrate).WithLogPath(runnerLogPath(worktreePath, "elasticsearch", "migrate", sourcePrefix))
		out, err := runner.Run(ctx, spec, worktreePath, sourcePrefix, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("migrate es %s: %w", sourcePrefix, err)
		}
		if out.ExitCode != 0 {
			emitRunnerError(ctx, st, repoID, worktreeID, "elasticsearch", sourcePrefix, "migrate", out)
			return fmt.Errorf("%s", runner.FormatError("migrate es", sourcePrefix, out))
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "migrate", stepStart)
	}
	if d.Seed != nil {
		stepStart := time.Now()
		out, err := runner.Run(ctx, runner.FromSeed(*d.Seed), worktreePath, sourcePrefix, tplCtx, inheritedEnv)
		if err != nil {
			return fmt.Errorf("seed es %s: %w", sourcePrefix, err)
		}
		if out.ExitCode != 0 {
			return fmt.Errorf("seed es %s exit %d: %s", sourcePrefix, out.ExitCode, out.StderrTail)
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "seed", stepStart)
	}
	return nil
}

// computeSnapshotKey hashes every input the snapshot fingerprint
// depends on (migration files, dump file, lockfile contents) and
// returns the canonical Key + the engine version it was built
// against. Pure helper — no engine I/O — so MySQL + Postgres share
// one path through the hashing primitives instead of duplicating
// the field assembly twice.
// computeSnapshotKey builds the content-addressed cache key for one
// database. It deliberately takes no source-DB name: the template's
// content depends only on the engine, version, and the hashed inputs
// (migrations, dumps, commands) — not the name it's restored into. See
// snapshot.FormatVersion v5.
func computeSnapshotKey(
	ctx context.Context,
	st *store.Store,
	d config.DatabaseConfig,
	worktreePath, engineVersion string,
) snapshot.Key {
	// 1. Hash every input under databases[].inputs[]. All inputs are
	//    content-hashed — the historical `filename` shortcut for
	//    append-only migration dirs is gone. doublestar (not stdlib
	//    filepath.Glob) so `**` matches zero-or-more path segments,
	//    including files sitting directly in the glob base dir.
	inputHashes := map[string]string{}
	for _, in := range d.Inputs {
		matches, _ := doublestar.FilepathGlob(filepath.Join(worktreePath, in.Glob))
		if len(matches) == 0 {
			continue
		}
		hs, err := snapshot.LockfileHashesForWithCache(ctx, st, matches)
		if err != nil {
			continue
		}
		// Fold per-file hashes into one entry per glob — keyed by the
		// glob so the user can tell from the SQLite row what changed.
		var sb strings.Builder
		for _, k := range sortedKeys(hs) {
			sb.WriteString(k + ":" + hs[k] + "\n")
		}
		inputHashes[in.Glob] = sb.String()
	}

	// 2. Hash every dump file in declared ORDER and fold them into a
	//    single DumpHashHex. Order matters: two dumps in different
	//    sequence can produce different DB state. For exactly ONE
	//    dump the result is byte-identical to the v5 single-dump hash
	//    (basename lookup against LockfileHashesForWithCache), so
	//    single-dump configs keep their existing templates instead of
	//    force-rebuilding when this array refactor lands.
	dumpHash := ""
	if len(d.Dump) > 0 {
		absPaths := make([]string, 0, len(d.Dump))
		for _, dn := range d.Dump {
			absPaths = append(absPaths, filepath.Join(worktreePath, dn.Path))
		}
		hashes, _ := snapshot.LockfileHashesForWithCache(ctx, st, absPaths)
		if len(d.Dump) == 1 {
			dumpHash = hashes[filepath.Base(absPaths[0])]
		} else {
			h := blake3.New(32, nil)
			for i, dn := range d.Dump {
				// Encode the user-declared (repo-relative) path so
				// duplicate basenames in different subdirs don't
				// collide, and so the worktree absolute path doesn't
				// leak into the hash (which would break cross-
				// worktree reuse — see snapshot.FormatVersion v5).
				_, _ = h.Write([]byte(dn.Path))
				_, _ = h.Write([]byte{0})
				_, _ = h.Write([]byte(hashes[filepath.Base(absPaths[i])]))
				_, _ = h.Write([]byte{0})
			}
			dumpHash = hex.EncodeToString(h.Sum(nil))
		}
	}

	// 3. Hash the migrate + seed run-strings + env maps. Changing
	//    what the user told us to run is itself an input change.
	cmdHash := commandsHash(d)

	// Stash command hash + dump hash in lockfileHashes (the snapshot
	// Key already has a string-map field for ancillary inputs).
	if cmdHash != "" {
		inputHashes["__commands__"] = cmdHash
	}
	return snapshot.New(d.Engine, engineVersion, "", "", dumpHash, inputHashes)
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
	key := computeSnapshotKey(ctx, st, d, worktreePath, engineVersion)
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

// computeInputVectors produces the per-input ordered file vectors
// stored alongside a snapshot so FindAncestorSnapshot can detect a
// content-prefix ancestor. Mirrors computeSnapshotKey's input walk:
// each `databases[].inputs[]` glob is expanded via doublestar, the
// matches are sorted by repo-relative path, and each entry carries
// the per-file content hash (looked up by absolute path so duplicate
// basenames in different subdirs don't collide). Empty/missing globs
// contribute no entry.
func computeInputVectors(ctx context.Context, st *store.Store, d config.DatabaseConfig, worktreePath string) map[string]store.InputVector {
	out := map[string]store.InputVector{}
	for _, in := range d.Inputs {
		matches, _ := doublestar.FilepathGlob(filepath.Join(worktreePath, in.Glob))
		if len(matches) == 0 {
			continue
		}
		hashByAbs, err := st.BatchHashedFiles(ctx, matches)
		if err != nil {
			continue
		}
		vec := make(store.InputVector, 0, len(matches))
		for _, abs := range matches {
			rel, relErr := filepath.Rel(worktreePath, abs)
			if relErr != nil {
				rel = filepath.Base(abs)
			}
			vec = append(vec, store.FileHash{Path: rel, Hash: hashByAbs[abs]})
		}
		sort.Slice(vec, func(i, j int) bool { return vec[i].Path < vec[j].Path })
		out[in.Glob] = vec
	}
	return out
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
	// branch_scoped databases tear down through the engine-agnostic
	// swap layer: capture the current branch into its durable copy,
	// drop the active namespace, keep the per-branch durable copies.
	if d.BranchScoped {
		return teardownBranchScoped(ctx, cfg, d, repoID, worktreeID, st)
	}
	switch fam, _ := engine.Canonical(d.Engine); fam {
	case engine.FamilyMySQL:
		return teardownMySQL(ctx, cfg, d, tplCtx, sl, repoID, worktreeID, st)
	case engine.FamilyPostgres:
		return teardownPostgres(ctx, cfg, d, tplCtx, sl, repoID, worktreeID, st)
	case engine.FamilyMongo:
		return teardownMongo(ctx, cfg, d, tplCtx, sl, repoID, worktreeID, st)
	case engine.FamilyRedis:
		return teardownRedis(ctx, cfg, d, tplCtx, sl, repoID, worktreeID, st)
	case engine.FamilyES:
		return teardownES(ctx, cfg, d, tplCtx, sl, repoID, worktreeID, st)
	}
	// Unknown engine — log + event so a typo'd `engine: postgress`
	// doesn't turn worktree teardown into a silent no-op (same
	// shape as the cold-build sibling-wipe that originally hid in
	// the event log).
	_ = st.WriteEvent(ctx, store.LevelWarn, "db_teardown_skipped",
		fmt.Sprintf("teardown skipped: unknown engine %q (allowed: %s)", d.Engine, engine.KnownList()),
		repoID, worktreeID, "", 0, map[string]any{
			"engine": d.Engine, "slug": sl,
		})
	return nil
}

// teardownMySQL drops the per-worktree MySQL database family. Extracted
// verbatim from teardownOne's `mysql` switch arm.
func teardownMySQL(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Mysql == nil {
		// Nothing to drop — engine isn't wired up. Silent skip
		// (matches the prepare side; the database was never
		// created in the first place).
		return nil
	}
	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close() }()
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
}

// teardownPostgres drops the per-worktree Postgres database family.
// Extracted verbatim from teardownOne's `postgres` switch arm.
func teardownPostgres(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Postgres == nil {
		return nil
	}
	drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close() }()
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
}

// teardownMongo drops the per-worktree MongoDB database family.
// Extracted verbatim from teardownOne's `mongodb` switch arm.
func teardownMongo(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Mongodb == nil {
		return nil
	}
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close(ctx) }()
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
}

// teardownRedis drops the per-worktree Redis key prefix. Extracted
// verbatim from teardownOne's `redis` switch arm.
func teardownRedis(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Redis == nil {
		return nil
	}
	if d.KeyPrefix == "" {
		return errors.New("redis: missing key_prefix")
	}
	drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close() }()
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
}

// teardownES drops the per-worktree Elasticsearch / OpenSearch index
// prefix. Extracted verbatim from teardownOne's `elasticsearch` arm.
func teardownES(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	if cfg.Connections.Elasticsearch == nil {
		return nil
	}
	if d.KeyPrefix == "" {
		return errors.New("es: missing key_prefix")
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
