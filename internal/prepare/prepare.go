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
	"log/slog"
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
	"github.com/stubbedev/treeman/internal/db/engineconn"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbfile "github.com/stubbedev/treeman/internal/db/filedb"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	dbs3 "github.com/stubbedev/treeman/internal/db/s3"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/migrations/runner"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
	"github.com/stubbedev/treeman/internal/wtlock"
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
	_ = st.WriteEvent(ctx, store.LevelError, store.EvtPrepareError,
		engine+"/"+runner.FormatError("phase="+label, sourceDB, out),
		repoID, worktreeID, label, 0, map[string]string{
			"engine":      engine,
			"source_db":   sourceDB,
			"exit_code":   strconv.Itoa(out.ExitCode),
			"stderr_tail": out.StderrTail,
			"stdout_tail": out.StdoutTail,
			"log_path":    out.LogPath,
		})
}

func runnerStepFailure(phase, target string, out runner.Outcome, err error) string {
	if err != nil {
		return fmt.Sprintf("%s %s: %v", phase, target, err)
	}
	return runner.FormatError(phase, target, out)
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
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPreparePhase,
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
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsStrategy,
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
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsCacheMiss,
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
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPreparePhase,
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
	// `seed:parent`, `seed:parent-snapshot`, `swap:resume`, `swap:parent`,
	// `swap:parent-snapshot`, `seed:parent-main`, `swap:parent-main`,
	// `swap:branch-point`, `adopt`,
	// `adopt:<default-branch>` (main checkout's first durable copy, see
	// adoptBranch), or `noop`.
	// Empty for the template/clone path.
	Decision string
	// Skipped is true when the engine's connection block is absent
	// from the resolved config — declaring `databases: [{engine:
	// mysql, ...}]` without `connections.mysql` is now a no-op
	// (logged via `prepare:skip`) instead of a worktree:create:error error.
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

// emitPrepareSkipped writes the `prepare:skip` info event used by
// the unconfigured-engine short-circuit and returns the matching
// Outcome. Single point so the event payload stays consistent across
// every engine.
func emitPrepareSkipped(ctx context.Context, st *store.Store, repoID, worktreeID int64, eng, sourceDB, reason string) Outcome {
	if st != nil {
		_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareSkip,
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
	// File copies: no connection budget to respect — the cap is disk
	// throughput, and reflink clones are metadata-only where supported.
	engine.FamilyFile: 8,
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
// Events emitted: `clones:start` (info) at entry, per-clone
// `clones:restore:end` (debug) / `clones:restore:error` (warn) inside
// each restore, and `clones:end` (info / error on failure) at exit
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
		_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtClonesStart,
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
			err := retryTransient(gctx, func() error { return restore(gctx, template, c) })
			dur := time.Since(cloneStart).Milliseconds()
			if err != nil {
				if isCancellation(err) {
					// A peer clone/engine failed and cancelled the shared
					// errgroup ctx; this clone was aborted, not failed.
					// Keep the collateral out of the failure count + warn
					// log — errgroup already carries the real cause.
					return err
				}
				failCount.Add(1)
				if st != nil {
					_ = st.WriteEvent(gctx, store.LevelWarn, store.EvtClonesRestoreError,
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
				_ = st.WriteEvent(gctx, store.LevelDebug, store.EvtClonesRestoreEnd,
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
		_ = st.WriteEvent(ctx, level, store.EvtClonesEnd,
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
// max-connections heuristic was used (surfaced in the clones:start
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
// A failed restore only condemns the cached template when the template
// itself is gone: then the helper emits snapshot_cache_fallback, drops
// the stale row and reports templateGone so the engine rebuilds. While
// the template still exists the failure lies with a target or the
// server (Too many connections, say) and is returned as is: rebuilding
// a fingerprint whose template is alive re-creates that template under
// every sibling worktree restoring from it, failing their restores
// with Unknown database (#170).
func cacheHitRestoreAndFanout(
	ctx context.Context,
	st *store.Store,
	repoID, worktreeID int64,
	exists func(context.Context, string) (bool, error),
	restore cloneRestorer,
	d config.DatabaseConfig,
	template, source string,
	clones []string,
	maxConns int,
	fingerprint string,
) (templateGone bool, err error) {
	targets := make([]string, 0, len(clones)+1)
	targets = append(targets, source)
	for _, c := range clones {
		if c != source {
			targets = append(targets, c)
		}
	}
	fanoutErr := fanOutClones(ctx, st, repoID, worktreeID, restore, template, targets, d.Engine, d.Fanout, maxConns)
	if fanoutErr == nil {
		return false, nil
	}
	if isCancellation(fanoutErr) {
		return false, fanoutErr
	}
	alive, existsErr := exists(ctx, template)
	if existsErr != nil {
		return false, fmt.Errorf("restore from %s: %w (probe template: %w)", template, fanoutErr, existsErr)
	}
	if alive {
		return false, fmt.Errorf("restore from cached template %s: %w", template, fanoutErr)
	}
	if st != nil {
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtSnapshotsCacheFallback,
			fmt.Sprintf("template gone mid-restore, rebuilding: %v", fanoutErr),
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      d.Engine,
				"template":    template,
				"fingerprint": fingerprint,
				"error":       fanoutErr.Error(),
			})
		if err := st.DeleteSnapshot(ctx, fingerprint); err != nil {
			return false, fmt.Errorf("forget %s snapshot %s whose template is gone: %w", d.Engine, template, err)
		}
	}
	return true, nil
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
	// Fresh restores every templated database and its test clones from
	// the cached template even when this worktree already holds them at
	// the current fingerprint, discarding whatever state they picked up
	// since (e.g. rows a previous CI build's tests left behind).
	Fresh bool
}

func (o RunOptions) Includes(dbIdx int) bool {
	return !o.FilterDBs || o.OnlyDBIndex == dbIdx
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
	// Serialise against any other prepare of this worktree — including
	// ones in other processes (`finalize --local`, the MCP server) that
	// the daemon's in-process withWorktreePrepare cannot see (#123).
	// Two builds of the same source database drop and reload it under
	// each other and leave it half-migrated.
	unlock, err := wtlock.Acquire(ctx, wtlock.Prepare, worktreePath, func() {
		if st != nil {
			_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareWait,
				"another prepare of this worktree is running; waiting for it to finish",
				repoID, worktreeID, "", 0, nil)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("wait for concurrent prepare: %w", err)
	}
	defer unlock()
	if opts.Fresh && st != nil {
		if err := st.ClearTemplateBuiltForWorktree(ctx, worktreeID); err != nil {
			return nil, fmt.Errorf("clear built-at fingerprints of worktree %d for a fresh prepare: %w", worktreeID, err)
		}
	}
	// Share engine version / max_connections probes across the parallel
	// per-database goroutines below — N databases on one engine pay one
	// round-trip instead of N.
	ctx = withProbeCache(ctx)
	tplCtx, err := withDBNamespaces(cfg, template.FromSlug(sl), worktreePath)
	if err != nil {
		return nil, err
	}

	results := make([]Outcome, len(cfg.Databases))
	hasResult := make([]bool, len(cfg.Databases))
	errs := make([]*DBError, len(cfg.Databases))
	// A plain Group, not errgroup.WithContext: databases are independent,
	// so one failing must not cancel its siblings mid-step (#119 — a dev
	// DB's failed migrate killed the testing DB's rollback halfway and
	// left it unmigrated). Every database runs to completion and the
	// failures are collected into one RunError.
	var g errgroup.Group
	for i, d := range cfg.Databases {
		if !opts.Includes(i) {
			continue
		}
		g.Go(func() error {
			o, err := prepareOneEngine(ctx, cfg, d, i, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
			if err == nil && !o.CacheHit && o.Fingerprint != "" && o.SourceDB != "" {
				err = recordTemplateBuilt(ctx, st, worktreeID, o.SourceDB, d.Engine, d.Connection, o.Fingerprint)
			}
			if err == nil {
				results[i] = o
				hasResult[i] = true
			} else {
				errs[i] = &DBError{Index: i, Engine: d.Engine, Err: err}
			}
			return nil
		})
	}
	_ = g.Wait()
	// Outcomes that did land are returned alongside any error so callers
	// can log a partial-success picture before surfacing it.
	var outcomes []Outcome
	for i, ok := range hasResult {
		if ok {
			outcomes = append(outcomes, results[i])
		}
	}
	var failed []*DBError
	for _, e := range errs {
		if e != nil {
			failed = append(failed, e)
		}
	}
	if len(failed) > 0 {
		return outcomes, &RunError{Failed: failed}
	}
	return outcomes, nil
}

// withDBNamespaces renders every database's namespace in this run's
// context and exposes them to step env as `{db:<index>}` (#132). cfg
// already carries the main-worktree overlay when the run targets the
// main worktree, so a sibling resolves to the name it actually has
// here rather than the linked-worktree template. File engines resolve
// to the same path `{target_db}` would give that database's own steps.
func withDBNamespaces(cfg *config.Config, tplCtx template.Context, worktreePath string) (template.Context, error) {
	dbs := make([]string, len(cfg.Databases))
	for i, d := range cfg.Databases {
		fam, _ := engine.Canonical(d.Engine)
		tmpl, field := d.NameTemplate, "name_template"
		if fam.Scope() == engine.ScopePrefix {
			tmpl, field = d.KeyPrefix, "key_prefix"
		}
		name, err := template.Render(tmpl, tplCtx)
		if err != nil {
			return tplCtx, fmt.Errorf("databases[%d].%s: %w", i, field, err)
		}
		if fam == engine.FamilyFile {
			if name, err = dbfile.SourcePathFor(cfg, d.Connection, worktreePath, name); err != nil {
				return tplCtx, fmt.Errorf("databases[%d]: %w", i, err)
			}
		}
		dbs[i] = name
	}
	return tplCtx.WithDBs(dbs), nil
}

// DBError is one database's failure inside the error RunFiltered
// returns. Index is the database's position in cfg.Databases.
type DBError struct {
	Index  int
	Engine string
	Err    error
}

func (e *DBError) Error() string {
	return fmt.Sprintf("databases[%d] (%s): %v", e.Index, e.Engine, e.Err)
}
func (e *DBError) Unwrap() error { return e.Err }

// RunError is the error RunFiltered returns: every database that failed,
// in cfg.Databases order. Each one still matches errors.Is / errors.As
// through Unwrap.
type RunError struct {
	Failed []*DBError
}

func (e *RunError) Error() string {
	msgs := make([]string, len(e.Failed))
	for i, f := range e.Failed {
		msgs[i] = f.Error()
	}
	return strings.Join(msgs, "\n")
}

func (e *RunError) Unwrap() []error {
	out := make([]error, len(e.Failed))
	for i, f := range e.Failed {
		out[i] = f
	}
	return out
}

// FailedDBIndices lists the cfg.Databases indices whose prepare failed
// in an error returned (possibly wrapped) by Run / RunFiltered. nil means
// err carries no per-database attribution, so the caller cannot tell
// which databases are healthy.
func FailedDBIndices(err error) []int {
	var re *RunError
	if !errors.As(err, &re) {
		return nil
	}
	idx := make([]int, len(re.Failed))
	for i, f := range re.Failed {
		idx[i] = f.Index
	}
	return idx
}

// prepareOneEngine dispatches one database to its engine prepare
// function. An unrecognised engine is surfaced as an event + a zero
// outcome, not a run failure.
func prepareOneEngine(
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
	fam, ok := engine.Canonical(d.Engine)
	if !ok {
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtPrepareUnsupported,
			fmt.Sprintf("engine=%s not recognised", d.Engine),
			repoID, worktreeID, "", 0, nil)
		return Outcome{}, nil
	}
	out, err := prepareFamily(ctx, fam, cfg, d, dbIdx, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
	maybeSpawnPrewarm(cfg, st, repoID, worktreeID, d, out, err)
	return out, err
}

// prepareFamily routes one database entry to its engine family's
// prepare path.
func prepareFamily(
	ctx context.Context,
	fam engine.Family,
	cfg *config.Config,
	d config.DatabaseConfig,
	dbIdx int,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	inheritedEnv map[string]string,
) (Outcome, error) {
	switch fam {
	case engine.FamilyMySQL:
		return prepareMySQL(ctx, cfg, d, dbIdx, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
	case engine.FamilyPostgres:
		return preparePostgres(ctx, cfg, d, dbIdx, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
	case engine.FamilyMongo:
		return prepareMongo(ctx, cfg, d, dbIdx, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
	case engine.FamilyRedis:
		return prepareRedis(ctx, cfg, d, dbIdx, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
	case engine.FamilyES:
		return prepareES(ctx, cfg, d, dbIdx, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
	case engine.FamilyS3:
		return prepareS3(ctx, cfg, d, dbIdx, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
	case engine.FamilyFile:
		return prepareFileEngine(ctx, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
	}
	return Outcome{}, nil
}

//nolint:funlen,cyclop // mirrors the linear cache-hit / incremental / cold-build / fanout flow used by every engine; extracting helpers just spreads the same conditions across functions
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
	mysqlConn, err := cfg.Connections.ResolveMysql(d.Connection)
	if err != nil {
		return Outcome{}, err
	}
	if mysqlConn == nil {
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, sourceDB,
			"connections.mysql not configured"), nil
	}

	drv, err := dbmysql.Connect(ctx, *mysqlConn)
	if err != nil {
		return Outcome{}, err
	}
	drv.SetPhysicalCloneMinBytes(d.PhysicalCloneMinBytes)
	defer func() { _ = drv.Close() }()

	// Probe keys ride the connection selector: two entries on one
	// family can target different servers (#44), so the shared probe
	// cache must not hand server A's version/max_conns to server B.
	version, _ := cachedProbe(ctx, "mysql-version:"+d.Connection, func() (string, error) { return drv.EngineVersion(ctx) })
	maxConns, _ := cachedProbe(ctx, "mysql-maxconns:"+d.Connection, func() (int, error) { return drv.MaxConnections(ctx) })

	// branch_scoped databases bypass the fingerprint template cache
	// entirely — their content is per-branch live data swapped through
	// the active namespace, not a migrations-only template shared
	// across worktrees.
	if d.BranchScoped {
		return runBranchScopedAt(ctx, version, branchScopedArgs{
			cfg: cfg, d: d, dbIdx: dbIdx, tplCtx: tplCtx, worktreePath: worktreePath,
			st: st, repoID: repoID, worktreeID: worktreeID, inheritedEnv: inheritedEnv,
			eng: &branchEngine{drv: mysqlNS{drv}, scope: scopeName, engine: "mysql"},
			loadDump: func(ctx context.Context, active string, dump dumpFile) error {
				_, e := dumpload.LoadMySQL(ctx, drv.DB, mysqlConn, active, dump.Path)
				return e
			},
		})
	}

	key, inputs, err := computeKeyAndVectors(ctx, st, d, worktreePath, version)
	if err != nil {
		return Outcome{}, err
	}
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
	// With a spare pool configured (#53), restores first claim a
	// pre-warmed spare via a physical clone of the spare — no logical
	// dump load — before falling back to the staged restore.
	restore := restoreFor(d.Engine, engineconn.NewMySQLConn(drv), drv.SnapshotRestoreStaged,
		st, repoID, worktreeID, d)
	out, done, flight, err := cacheHitGeneric(
		ctx,
		drv.DatabaseExists,
		restore,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourceDB,
		key,
		maxConns,
		started,
	)
	defer flight.finish()
	if done || err != nil {
		return out, err
	}

	// Per-input vectors used for ancestor lookup AND persisted into
	// the new snapshot row so future preps can build incrementally
	// off this one.

	ops := incrementalOps{
		exists:                drv.DatabaseExists,
		snapshotRestore:       drv.SnapshotRestore,
		snapshotRestoreFanout: drv.SnapshotRestoreStaged,
		snapshotCreate: func(ctx context.Context, src, tmpl string) error {
			if cerr := drv.SnapshotCreate(ctx, src, tmpl); cerr != nil {
				return cerr
			}
			emitCloneStrategy(ctx, st, repoID, worktreeID, d.Engine, src, tmpl,
				string(drv.LastCloneStrategy()))
			return nil
		},
	}
	dumpKey := dumpOnlySnapshotKey(d.Engine, d.Connection, version, key.DumpHashHex)

	// Incremental path: try to find a content-PREFIX ancestor template
	// (same engine/version/dump/commands; every input vector a prefix
	// of this run's) and clone + migrate from there instead of a full
	// cold build. The migration framework's own ledger skips the
	// already-applied files so only the new ones run.
	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, maxConns, key, inputs,
		inheritedEnv, started, ops)
	if done || err != nil {
		return out, err
	}

	// Rollback path (opt-in): an EXISTING migration's content changed
	// mid-sequence, so no prefix ancestor matched. If a `rollback:`
	// command is configured, clone the diverging ancestor, unwind the
	// changed tail, and re-migrate forward. Candidates whose tail the
	// branch cannot unwind (a template built from a source ahead of the
	// branch, or a removed migration file) are rejected upstream in
	// FindRollbackAncestor and fall through to cold. Hard-falls-back to
	// cold on any error.
	out, done, err = tryRollbackIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, maxConns, key, inputs,
		inheritedEnv, started, ops)
	if done || err != nil {
		return out, err
	}

	// Dump-only fast path: clone the post-dump intermediate template and
	// run the full forward migrate+seed — same result as a cold build
	// but skips reloading the dump. No dump-only template yet → falls
	// through to the cold build below (which seeds it).
	out, done, err = tryDumpOnlyBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, maxConns, key, dumpKey,
		inputs, inheritedEnv, started, ops)
	if done || err != nil {
		return out, err
	}

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareStart,
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
	if err := mysqlColdBuildSteps(
		ctx,
		drv,
		mysqlConn,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourceDB,
		version,
		dumpKey,
		inheritedEnv,
	); err != nil {
		return Outcome{}, err
	}

	// Template + clones: logical strategy can be fanned out from source
	// in one parallel wave (template + N clones together) because logical
	// INSERT…SELECT doesn't share a per-source FLUSH lock. The physical
	// strategy keeps the 2-step path (build template, then staged restore
	// to clones) where the staging dir absorbs the FOR EXPORT
	// serialization that N+1 concurrent physical clones would otherwise
	// hit on the source.
	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if drv.PreferLogicalFor(ctx, sourceDB) {
		if err := mysqlMergedColdFanout(ctx, drv, cfg, d, st, repoID, worktreeID,
			sourceDB, templateName, version, key, inputs, clones, maxConns); err != nil {
			return Outcome{}, err
		}
	} else {
		if err := mysqlSnapshotAndRecord(
			ctx,
			drv,
			cfg,
			d,
			st,
			repoID,
			worktreeID,
			sourceDB,
			templateName,
			version,
			key,
			inputs,
		); err != nil {
			return Outcome{}, err
		}
		if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestoreStaged,
			templateName, clones, d.Engine, d.Fanout, maxConns); err != nil {
			return Outcome{}, err
		}
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
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

// cacheHitGeneric is the engine-agnostic fingerprint-cache fast path
// shared by every engine. Returns (out, true, flight, nil) when the
// cached template was reused and the caller should return `out`;
// (Outcome{}, false, flight, nil) when the cache missed or the template
// vanished mid-flight (caller must cold-build; flight releases the
// cross-worktree build slot when the engine function returns — callers
// `defer flight.finish()` unconditionally, which is safe on the zero
// value by construction); (Outcome{}, false, flight, err) on a hard
// error.
//
// `exists` is the driver's namespace-presence probe (DatabaseExists for
// name-scoped engines, PrefixExists/IndexExists for prefix-scoped ones)
// and `restore` is its template→target clone. `maxConns` bounds the
// clone fan-out (0 for engines with no connection cap). Behaviour is
// identical across engines — only these two closures, the connection
// cap, and the engine label vary, so the five per-engine copies collapse
// into this one.
func cacheHitGeneric(
	ctx context.Context,
	exists func(context.Context, string) (bool, error),
	restore func(context.Context, string, string) error,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB string,
	key snapshot.Key,
	maxConns int,
	started time.Time,
) (Outcome, bool, buildFlight, error) {
	rec, err := lookupCachedSnapshot(ctx, st, d.Engine, key.Fingerprint(), func() {
		emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourceDB, key.Fingerprint(), cacheMissNoRow)
	})
	if err != nil {
		return Outcome{}, false, buildFlight{}, err
	}
	if rec == nil {
		return Outcome{}, false, beginBuild(d.Engine, key.Fingerprint()), nil
	}
	alive, err := liveSnapshot(ctx, st, exists, d.Engine, rec)
	if err != nil {
		return Outcome{}, false, buildFlight{}, err
	}
	if !alive {
		emitCacheMiss(ctx, st, repoID, worktreeID, d.Engine, sourceDB, key.Fingerprint(), cacheMissTemplateGone)
		return Outcome{}, false, beginBuild(d.Engine, key.Fingerprint()), nil
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsCacheHit,
		"template="+rec.TemplateName,
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      d.Engine,
			"source_db":   sourceDB,
			"template":    rec.TemplateName,
			"fingerprint": key.Fingerprint(),
		})
	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, false, buildFlight{}, err
	}
	// Per-worktree built-at gate: when this worktree's namespaces were
	// already populated from THIS fingerprint and they all still exist,
	// the restore + fan-out is the no-op "cache hit" promises. (Without
	// it, every finalize re-restored the source + every clone from the
	// template even though the inputs hadn't changed: ~90s of I/O on a
	// 16-clone repo per HEAD move.)
	restored, err := templateBuiltTargets(
		ctx,
		st,
		exists,
		worktreeID,
		sourceDB,
		d.Engine,
		d.Connection,
		clones,
		key.Fingerprint(),
	)
	if err != nil {
		return Outcome{}, false, buildFlight{}, err
	}
	if restored {
		ms := time.Since(started).Milliseconds()
		_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
			fmt.Sprintf("cache_hit clones=%d restored=0 duration=%dms", len(clones), ms),
			repoID, worktreeID, "", 0, map[string]string{
				"engine":      d.Engine,
				"source_db":   sourceDB,
				"template":    rec.TemplateName,
				"clones":      strconv.Itoa(len(clones)),
				"fingerprint": key.Fingerprint(),
				"cache_hit":   "true",
				"restored":    "0",
				"duration_ms": strconv.FormatInt(ms, 10),
			})
		return Outcome{
			Engine:       d.Engine,
			SourceDB:     sourceDB,
			TemplateName: rec.TemplateName,
			Fingerprint:  key.Fingerprint(),
			CacheHit:     true,
			Clones:       clones,
		}, true, buildFlight{}, nil
	}
	// Cache-hit skips the cold-build path that populates the source DB
	// via dump+migrate. Restore source from the template so non-parallel
	// runs (a single test command, dev shells — anything that reads the
	// user-facing name_template without a per-worker suffix) see a
	// populated DB. The source is folded into the clone fan-out so it's
	// repopulated in parallel with the clones rather than serially first.
	//
	// If the template disappears mid-flight (race with EvictExcess
	// between the existence probe and the restore/fanout), rebuild it
	// under a build flight so sibling worktrees wait for the rebuild
	// instead of racing their own.
	templateGone, err := cacheHitRestoreAndFanout(ctx, st, repoID, worktreeID, exists, restore,
		d, rec.TemplateName, sourceDB, clones, maxConns, key.Fingerprint())
	if err != nil {
		return Outcome{}, false, buildFlight{}, err
	}
	if templateGone {
		return Outcome{}, false, beginBuild(d.Engine, key.Fingerprint()), nil
	}
	if err := recordTemplateBuilt(ctx, st, worktreeID, sourceDB, d.Engine, d.Connection, key.Fingerprint()); err != nil {
		return Outcome{}, false, buildFlight{}, err
	}
	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
		fmt.Sprintf("cache_hit clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      d.Engine,
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
	}, true, buildFlight{}, nil
}

// templateBuiltTargets reports whether this worktree's source + clone
// namespaces were already built at `fingerprint` and all of them still
// exist on the engine (cheap per-target existence probes). Returns
// true when the restore can be skipped entirely.
func templateBuiltTargets(
	ctx context.Context,
	st *store.Store,
	exists func(context.Context, string) (bool, error),
	worktreeID int64,
	sourceDB, engine, connection string,
	clones []string,
	fingerprint string,
) (bool, error) {
	builtFP, ok, err := st.GetTemplateBuilt(ctx, worktreeID, sourceDB, engine, connection)
	if err != nil {
		return false, fmt.Errorf("read the built-at fingerprint of %s %s: %w", engine, sourceDB, err)
	}
	if !ok || builtFP != fingerprint {
		return false, nil
	}
	targets := make([]string, 0, len(clones)+1)
	targets = append(targets, sourceDB)
	for _, c := range clones {
		if c != sourceDB {
			targets = append(targets, c)
		}
	}
	for _, t := range targets {
		present, err := exists(ctx, t)
		if err != nil {
			return false, fmt.Errorf("probe %s %s: %w", engine, t, err)
		}
		if !present {
			return false, nil
		}
	}
	return true, nil
}

func lookupCachedSnapshot(ctx context.Context, st *store.Store, engine, fingerprint string, onMiss func()) (*store.SnapshotRecord, error) {
	rec, err := st.LookupAndTouchSnapshot(ctx, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("look up the %s snapshot %s: %w", engine, fingerprint, err)
	}
	if rec != nil {
		return rec, nil
	}
	onMiss()
	if !waitForBuild(engine, fingerprint) {
		return nil, nil
	}
	rec, err = st.LookupAndTouchSnapshot(ctx, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("look up the %s snapshot %s: %w", engine, fingerprint, err)
	}
	return rec, nil
}

func runBranchScopedAt(ctx context.Context, engineVersion string, a branchScopedArgs) (Outcome, error) {
	key, err := computeSnapshotKey(ctx, a.st, a.d, a.worktreePath, engineVersion)
	if err != nil {
		return Outcome{}, err
	}
	a.migrateFP = key.Fingerprint()
	return runBranchScoped(ctx, a)
}

func liveSnapshot(
	ctx context.Context,
	st *store.Store,
	exists func(context.Context, string) (bool, error),
	engine string,
	rec *store.SnapshotRecord,
) (bool, error) {
	alive, err := exists(ctx, rec.TemplateName)
	if err != nil {
		return false, fmt.Errorf("probe %s template %s: %w", engine, rec.TemplateName, err)
	}
	if alive {
		return true, nil
	}
	if err := st.DeleteSnapshot(ctx, rec.Fingerprint); err != nil {
		return false, fmt.Errorf("forget %s snapshot %s whose template is gone: %w", engine, rec.TemplateName, err)
	}
	return false, nil
}

func recordTemplateBuilt(ctx context.Context, st *store.Store, worktreeID int64, sourceDB, engine, connection, fingerprint string) error {
	if err := st.SetTemplateBuilt(ctx, worktreeID, sourceDB, engine, connection, fingerprint); err != nil {
		return fmt.Errorf("record that %s %s was built at %s: %w", engine, sourceDB, fingerprint, err)
	}
	return nil
}

// runPhase runs one cold-build runner phase (migrate or seed) against
// `target` (the source DB name or key/index prefix), with the full
// instrumentation every engine now shares: a per-phase log file, a
// `prepare_runner_error` event + formatted error on a non-zero exit, and
// a `prepare_phase` done event with the step duration. `phase` is
// "migrate" or "seed". A nil-spec phase is the caller's concern (guard on
// d.Migrate / d.Seed). The engine label is d.Engine throughout so logs,
// events, and the log path all agree on the configured engine name.
//
// NOTE: the incremental-build path (tryIncrementalBuild) keeps its own
// migrate invocation — its error wording and event flow are deliberately
// distinct ("incremental migrate") and must not be folded in here.
func runPhase(
	ctx context.Context,
	st *store.Store,
	repoID, worktreeID int64,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath, target, phase string,
	spec runner.Spec,
	inheritedEnv map[string]string,
) error {
	stepStart := time.Now()
	spec = spec.WithLogPath(runnerLogPath(worktreePath, d.Engine, phase, target))
	out, err := runner.Run(ctx, spec, worktreePath, target, tplCtx, inheritedEnv)
	if err != nil {
		return fmt.Errorf("%s %s: %w", phase, target, err)
	}
	if out.ExitCode != 0 {
		emitRunnerError(ctx, st, repoID, worktreeID, d.Engine, target, phase, out)
		return fmt.Errorf("%s", runner.FormatError(phase+" source", target, out))
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, target, phase, stepStart)
	return nil
}

// mysqlColdBuildSteps runs the MySQL cold-build populate phase: drop +
// recreate the source DB, optionally load a dump, then run migrate and
// seed.
func mysqlColdBuildSteps(
	ctx context.Context,
	drv *dbmysql.Driver,
	mc *config.MysqlConn,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB, version string,
	dumpKey snapshot.Key,
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
		strategy, err := dumpload.LoadMySQL(ctx, drv.DB, mc, sourceDB, dr.Path)
		if err != nil {
			return fmt.Errorf("load dump %s: %w", dr.Path, err)
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourceDB, dr.Path, i, len(dumps), stepStart, string(strategy))
	}
	if err := seedDumpOnlyTemplate(ctx, st, d, repoID, worktreeID, sourceDB, version, dumpKey, len(dumps),
		drv.DatabaseExists,
		func(ctx context.Context, src, tmpl string) error { return drv.SnapshotCreate(ctx, src, tmpl) }); err != nil {
		return err
	}
	if d.Migrate != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourceDB,
			"migrate",
			runner.FromMigrate(*d.Migrate),
			inheritedEnv,
		); err != nil {
			return err
		}
	}
	if d.Seed != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourceDB,
			"seed",
			runner.FromSeed(*d.Seed),
			inheritedEnv,
		); err != nil {
			return err
		}
	}
	return nil
}

// mysqlSnapshotAndRecord builds the template snapshot from the populated
// source, records the SQLite snapshot row, and fires the detached LRU
// eviction sweep. Extracted verbatim from prepareMySQL's snapshot/record
// block.
// recordSnapshot writes the SQLite snapshot row for a freshly-built
// template. Identical across every engine and across both the cold-build
// and incremental paths — only the engine label, version, source/template
// names, and input vectors vary.
func recordSnapshot(
	ctx context.Context,
	st *store.Store,
	key snapshot.Key,
	engine, version, source, template string,
	inputs map[string]store.InputVector,
	repoID int64,
) error {
	err := st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    key.Fingerprint(),
		Engine:         engine,
		EngineVersion:  version,
		SourceDB:       source,
		TemplateName:   template,
		MigrationsHash: key.MigrationsHashHex,
		DumpHash:       key.DumpHashHex,
		LockfileHashes: key.LockfileHashes,
		Inputs:         inputs,
		RepoID:         repoID,
	})
	if err != nil {
		return fmt.Errorf("record %s snapshot %s of %s: %w", engine, template, source, err)
	}
	return nil
}

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
	if err := recordSnapshot(ctx, st, key, d.Engine, version, sourceDB, templateName, inputs, repoID); err != nil {
		return err
	}
	// Fire-and-forget LRU eviction for this repo. Uses a fresh
	// background context with a hard deadline so a stalled DROP
	// (e.g. lock contention) can't leak this goroutine forever.
	// Errors are logged inside EvictExcess.
	spawnEvict(cfg, st, repoID)
	return nil
}

// mysqlMergedColdFanout fans source → [template + clones...] in ONE
// parallel wave when the strategy selector picked logical. Saves the
// serial snapshot-create-then-restore round trip the 2-step path pays
// (~10s on a 200-table schema) and keeps every target reading from the
// freshly-built source DB while its pages are still hot in the buffer
// pool. Mirrors the build_helper.d prepare flow that originally
// inspired this codebase: 9 concurrent INSERT…SELECT clones from one
// source, capped by the same auto-tuned connection budget as
// fanOutClones.
//
// Logical clones don't share a per-source FLUSH lock, so concurrent
// SnapshotCreate from the same source is safe; the physical path's
// FLUSH TABLES … FOR EXPORT would serialize, which is why the
// dispatcher keeps it on the 2-step staged path.
func mysqlMergedColdFanout(
	ctx context.Context,
	drv *dbmysql.Driver,
	cfg *config.Config,
	d config.DatabaseConfig,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB, templateName, version string,
	key snapshot.Key,
	inputs map[string]store.InputVector,
	clones []string,
	maxConns int,
) error {
	targets := make([]string, 0, len(clones)+1)
	targets = append(targets, templateName)
	targets = append(targets, clones...)

	limit, autoTuned := fanOutLimit(d.Fanout, maxConns, len(targets), d.Engine)
	started := time.Now()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtClonesStart,
		fmt.Sprintf("engine=%s source=%s targets=%d (template+%d clones) limit=%d", d.Engine, sourceDB, len(targets), len(clones), limit),
		repoID, worktreeID, "", 0, map[string]any{
			"engine":     d.Engine,
			"source":     sourceDB,
			"template":   templateName,
			"targets":    len(targets),
			"limit":      limit,
			"auto_tuned": autoTuned,
			"max_conns":  maxConns,
			"merged":     true,
		})

	var (
		okCount   atomic.Uint32
		failCount atomic.Uint32
		slowestMs atomic.Int64
		slowestMu sync.Mutex
		slowestDB string
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(limit)
	for _, tgt := range targets {
		g.Go(func() error {
			cloneStart := time.Now()
			err := drv.SnapshotCreate(gctx, sourceDB, tgt)
			dur := time.Since(cloneStart).Milliseconds()
			if err != nil {
				failCount.Add(1)
				_ = st.WriteEvent(gctx, store.LevelWarn, store.EvtClonesRestoreError,
					fmt.Sprintf("engine=%s source=%s db=%s err=%v", d.Engine, sourceDB, tgt, err),
					repoID, worktreeID, "", dur, map[string]any{
						"engine": d.Engine,
						"source": sourceDB,
						"db":     tgt,
						"error":  err.Error(),
					})
				return fmt.Errorf("snapshot create %s → %s: %w", sourceDB, tgt, err)
			}
			okCount.Add(1)
			_ = st.WriteEvent(gctx, store.LevelDebug, store.EvtClonesRestoreEnd,
				fmt.Sprintf("engine=%s db=%s", d.Engine, tgt),
				repoID, worktreeID, "", dur, map[string]any{
					"engine": d.Engine,
					"source": sourceDB,
					"db":     tgt,
				})
			if cur := slowestMs.Load(); dur > cur {
				slowestMu.Lock()
				if dur > slowestMs.Load() {
					slowestMs.Store(dur)
					slowestDB = tgt
				}
				slowestMu.Unlock()
			}
			return nil
		})
	}
	gErr := g.Wait()

	totalMs := time.Since(started).Milliseconds()
	level := store.LevelInfo
	if failCount.Load() > 0 {
		level = store.LevelError
	}
	slowestMu.Lock()
	slowDB := slowestDB
	slowestMu.Unlock()
	_ = st.WriteEvent(ctx, level, store.EvtClonesEnd,
		fmt.Sprintf("engine=%s ok=%d fail=%d slowest=%dms (merged)", d.Engine, okCount.Load(), failCount.Load(), slowestMs.Load()),
		repoID, worktreeID, "", totalMs, map[string]any{
			"engine":     d.Engine,
			"source":     sourceDB,
			"template":   templateName,
			"ok":         okCount.Load(),
			"fail":       failCount.Load(),
			"slowest_ms": slowestMs.Load(),
			"slowest_db": slowDB,
			"merged":     true,
		})
	if gErr != nil {
		return gErr
	}

	// Synthetic snapshot-create event so downstream observers (and the
	// existing TestMySQLEndToEnd-style assertions) still see the
	// template's strategy + a per-template phase boundary.
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "snapshot-create", started)
	emitCloneStrategy(ctx, st, repoID, worktreeID, d.Engine, sourceDB, templateName, string(drv.LastCloneStrategy()))
	if err := recordSnapshot(ctx, st, key, d.Engine, version, sourceDB, templateName, inputs, repoID); err != nil {
		return err
	}
	spawnEvict(cfg, st, repoID)
	return nil
}

// spawnEvict fires LRU snapshot eviction for a repo in the background,
// as store-owned work: it outlives the prepare request's own context,
// and the store's Close waits for it. A hard 5-minute deadline means a
// stalled DROP (e.g. engine lock contention) can never hold it forever.
// Errors are logged inside EvictExcess.
func spawnEvict(cfg *config.Config, st *store.Store, repoID int64) {
	if st == nil {
		return
	}
	st.Go("snapshot:evict", "", func() {
		evictCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		snapshot.EvictExcess(evictCtx, cfg, st, repoID)
	})
}

// buildFlights dedupes concurrent cold builds of the same (engine,
// fingerprint) template across worktrees — two worktrees preparing a
// missing fingerprint both used to run the full dump+migrate+snapshot
// pipeline in parallel; now the second waits for the first's snapshot
// row and takes the restore path instead. Same shape as prewarm's
// in-flight map.
var buildFlights = struct {
	sync.Mutex
	m map[string]chan struct{}
}{m: map[string]chan struct{}{}}

func buildFlightKey(engine, fingerprint string) string {
	return engine + "/" + fingerprint
}

// waitForBuild blocks while another prepare is cold-building this
// (engine, fingerprint), returning true. False when nothing is in
// flight (no wait happened).
func waitForBuild(engine, fingerprint string) bool {
	buildFlights.Lock()
	ch, ok := buildFlights.m[buildFlightKey(engine, fingerprint)]
	buildFlights.Unlock()
	if !ok {
		return false
	}
	<-ch
	return true
}

// buildFlight releases a cold build's in-flight slot. The zero value
// is a completed no-op flight, so a cacheHitGeneric result can always
// be `defer flight.finish()`-ed — the nil-func variant of this
// contract compiled fine and crashed every e2e suite in v2.5.91; a
// struct with a nil-safe method cannot regress that way.
type buildFlight struct {
	release func()
}

func (f buildFlight) finish() {
	if f.release != nil {
		f.release()
	}
}

// beginBuild marks a cold build in flight and returns the flight whose
// finish() the caller must defer — it unblocks waiters and clears the
// slot when the engine function returns (success or failure). finish
// is idempotent (once-guarded), so a defensive extra call is harmless.
func beginBuild(engine, fingerprint string) buildFlight {
	key := buildFlightKey(engine, fingerprint)
	ch := make(chan struct{})
	buildFlights.Lock()
	buildFlights.m[key] = ch
	buildFlights.Unlock()
	var once sync.Once
	return buildFlight{release: func() {
		once.Do(func() {
			buildFlights.Lock()
			delete(buildFlights.m, key)
			buildFlights.Unlock()
			close(ch)
		})
	}}
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
	// Used for the single ancestor→source restore — keep it on the
	// direct one-copy path.
	snapshotRestore cloneRestorer
	// snapshotRestoreFanout, when non-nil, restores the freshly-built
	// template into the test clones; falls back to snapshotRestore when
	// nil. Lets mysql use its export-once staged restore for the
	// many-clone fan-out without paying the extra staging copy on the
	// single ancestor→source restore above.
	snapshotRestoreFanout cloneRestorer
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
//nolint:gocyclo // one self-contained orchestrator with explicit fall-through events — splitting just spreads the same branches across helpers without simplifying review
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
	anc, err := st.FindAncestorSnapshot(ctx, repoID, d.Engine, version, key.DumpHashHex, commandsHash, inputs)
	if err != nil {
		return Outcome{}, false, fmt.Errorf("find an ancestor %s snapshot: %w", d.Engine, err)
	}
	if anc == nil {
		return Outcome{}, false, nil
	}
	// Pin the ancestor so a concurrent GC can't evict it while we're
	// in the middle of cloning + migrating from it.
	unpinAnc := snapshot.Pin(anc.Fingerprint)
	defer unpinAnc()

	if live, err := liveSnapshot(ctx, st, ops.exists, d.Engine, anc); !live || err != nil {
		return Outcome{}, false, err
	}

	delta := vectorDelta(anc.Inputs, inputs)
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareIncrementalStart,
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
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtPrepareIncrementalFallback,
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
	if err := recordSnapshot(ctx, st, key, d.Engine, version, source, templateName, inputs, repoID); err != nil {
		return Outcome{}, false, err
	}
	spawnEvict(cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, false, err
	}
	fanoutRestore := ops.snapshotRestore
	if ops.snapshotRestoreFanout != nil {
		fanoutRestore = ops.snapshotRestoreFanout
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, fanoutRestore, templateName,
		clones, d.Engine, d.Fanout, maxConns); err != nil {
		return Outcome{}, false, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
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

// dumpOnlySnapshotKey builds the cache key for the post-dump, pre-
// migrate intermediate template. Keyed ONLY on (engine, connection,
// version, dumpHash) plus a marker so it never collides with a real
// template and is excluded from ancestor lookups (see
// store.DumpOnlyMarkerKey).
func dumpOnlySnapshotKey(engineName, connection, version, dumpHash string) snapshot.Key {
	return snapshot.New(engineName, version, connection, "", "", dumpHash,
		map[string]string{store.DumpOnlyMarkerKey: "1"})
}

// seedDumpOnlyTemplate snapshots the freshly dump-loaded source into the
// dump-only template so a future migration edit can rebuild from it
// without reloading the dump. Best-effort and a no-op unless there's a
// dump AND a migrate step (without migrate the dump-only template would
// equal the real template). Failures are logged, never fatal — the cold
// build proceeds and the fast path is simply unavailable next time.
func seedDumpOnlyTemplate(
	ctx context.Context,
	st *store.Store,
	d config.DatabaseConfig,
	repoID, worktreeID int64,
	sourceDB, version string,
	dumpKey snapshot.Key,
	dumpCount int,
	exists func(context.Context, string) (bool, error),
	snapshotCreate func(ctx context.Context, src, tmpl string) error,
) error {
	if dumpCount == 0 || d.Migrate == nil {
		return nil
	}
	dumpTpl := dumpKey.TemplateName()
	rec, err := st.LookupSnapshot(ctx, dumpKey.Fingerprint())
	if err != nil {
		return fmt.Errorf("look up the dump-only %s template %s: %w", d.Engine, dumpTpl, err)
	}
	if rec != nil {
		if alive, aerr := exists(ctx, rec.TemplateName); aerr == nil && alive {
			return nil
		}
	}
	start := time.Now()
	if err := snapshotCreate(ctx, sourceDB, dumpTpl); err != nil {
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtPrepareDumpOnlyFallback,
			fmt.Sprintf("seed dump-only template failed: %v", err),
			repoID, worktreeID, "", 0, map[string]string{
				"engine": d.Engine, "source_db": sourceDB, "template": dumpTpl,
				"error": err.Error(),
			})
		return nil
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "dumponly-snapshot", start)
	return recordSnapshot(ctx, st, dumpKey, d.Engine, version, sourceDB, dumpTpl, nil, repoID)
}

// snapshotRecordFanout is the shared tail of the incremental build
// paths: create the template from the populated source, record its
// snapshot row, fire LRU eviction, then fan out the test clones.
func snapshotRecordFanout(
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
	ops incrementalOps,
) ([]string, error) {
	snapStart := time.Now()
	if err := ops.snapshotCreate(ctx, source, templateName); err != nil {
		return nil, fmt.Errorf("snapshot create %s → %s: %w", source, templateName, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, source, "snapshot-create", snapStart)
	if err := recordSnapshot(ctx, st, key, d.Engine, version, source, templateName, inputs, repoID); err != nil {
		return nil, err
	}
	spawnEvict(cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return nil, err
	}
	fanoutRestore := ops.snapshotRestore
	if ops.snapshotRestoreFanout != nil {
		fanoutRestore = ops.snapshotRestoreFanout
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, fanoutRestore, templateName,
		clones, d.Engine, d.Fanout, maxConns); err != nil {
		return nil, err
	}
	return clones, nil
}

// tryDumpOnlyBuild rebuilds from the post-dump intermediate template
// (see dumpOnlySnapshotKey): clone it into the source, then run the FULL
// forward migrate + seed. This is correctness-identical to a cold build
// (it never runs down()), it just skips reloading the dump — the usual
// dominant cost. Returns (out, true, nil) on success; (Outcome{}, false,
// nil) when no dump-only template exists yet (caller cold-builds, which
// seeds it). A migrate/seed failure is propagated as a real error: a
// cold build would re-run the same forward commands and fail identically.
func tryDumpOnlyBuild(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	source, templateName, version string,
	maxConns int,
	key, dumpKey snapshot.Key,
	inputs map[string]store.InputVector,
	inheritedEnv map[string]string,
	started time.Time,
	ops incrementalOps,
) (Outcome, bool, error) {
	if len(d.Dump) == 0 || d.Migrate == nil {
		return Outcome{}, false, nil
	}
	// Touch the row on use (LRU): a hot dump-only template must not age
	// out of the cache — a migration edit then silently falls back to a
	// full dump reload, the dominant cold cost.
	anc, err := st.LookupAndTouchSnapshot(ctx, dumpKey.Fingerprint())
	if err != nil {
		return Outcome{}, false, fmt.Errorf("look up the dump-only %s snapshot: %w", d.Engine, err)
	}
	if anc == nil {
		return Outcome{}, false, nil
	}
	unpin := snapshot.Pin(anc.Fingerprint)
	defer unpin()

	if live, err := liveSnapshot(ctx, st, ops.exists, d.Engine, anc); !live || err != nil {
		return Outcome{}, false, err
	}

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareDumpOnlyStart,
		fmt.Sprintf("engine=%s dump_template=%s", d.Engine, anc.TemplateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": d.Engine, "source_db": source, "template": templateName,
			"fingerprint": key.Fingerprint(), "dump_template": anc.TemplateName,
		})

	restoreStart := time.Now()
	if err := ops.snapshotRestore(ctx, anc.TemplateName, source); err != nil {
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtPrepareDumpOnlyFallback,
			fmt.Sprintf("dump-only restore failed: %v", err),
			repoID, worktreeID, "", 0, map[string]string{
				"engine": d.Engine, "dump_template": anc.TemplateName, "error": err.Error(),
			})
		return Outcome{}, false, nil
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, source, "dumponly-restore", restoreStart)

	if err := runPhase(ctx, st, repoID, worktreeID, d, tplCtx, worktreePath, source,
		"migrate", runner.FromMigrate(*d.Migrate), inheritedEnv); err != nil {
		return Outcome{}, false, err
	}
	if d.Seed != nil {
		if err := runPhase(ctx, st, repoID, worktreeID, d, tplCtx, worktreePath, source,
			"seed", runner.FromSeed(*d.Seed), inheritedEnv); err != nil {
			return Outcome{}, false, err
		}
	}

	clones, err := snapshotRecordFanout(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, source, templateName, version, maxConns, key, inputs, ops)
	if err != nil {
		return Outcome{}, false, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
		fmt.Sprintf("dump_only clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": d.Engine, "source_db": source, "template": templateName,
			"clones": strconv.Itoa(len(clones)), "cache_hit": "false",
			"dump_only": "true", "duration_ms": strconv.FormatInt(ms, 10),
		})

	return Outcome{
		Engine: d.Engine, SourceDB: source, TemplateName: templateName,
		Fingerprint: key.Fingerprint(), CacheHit: false, Clones: clones,
		IncrementalBase: anc.Fingerprint,
	}, true, nil
}

// tryRollbackIncrementalBuild handles an edit to an ALREADY-APPLIED
// migration (its content changed mid-sequence, so no prefix ancestor
// matched). Opt-in via `databases[].rollback`. It clones the diverging
// ancestor template, runs the rollback command to unwind the changed
// tail (TREEMAN_ROLLBACK_STEPS migrations), then re-runs migrate forward
// — the only path that re-applies an edit whose ledger row is baked into
// the dump. Ancestors whose tail the branch has no migration files for
// (a source ahead of the branch, a removed file) are rejected by
// FindRollbackAncestor — unwinding them would silently no-op ("Migration
// not found") and poison the cached template. ANY rollback/migrate/seed
// error falls through to the dump-only or cold build (returns
// (Outcome{}, false, nil); both rebuild the source from the dump). The
// failure is recorded as a warn-level prepare:rollback:fallback, never
// a prepare:error: the run recovers, so it must not read as terminal.
//
// WARNING: rollback runs the migration's CURRENT down(), not the one
// that matched the applied schema — see the config doc on Rollback.
func tryRollbackIncrementalBuild(
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
	if d.Rollback == nil || d.Migrate == nil {
		return Outcome{}, false, nil
	}
	commandsHash := key.LockfileHashes[store.CommandsHashKey]
	anc, steps, err := st.FindRollbackAncestor(ctx, repoID, d.Engine, version,
		key.DumpHashHex, commandsHash, inputs)
	if err != nil {
		return Outcome{}, false, fmt.Errorf("find a rollback ancestor %s snapshot: %w", d.Engine, err)
	}
	if anc == nil || steps <= 0 {
		return Outcome{}, false, nil
	}
	unpin := snapshot.Pin(anc.Fingerprint)
	defer unpin()

	if live, err := liveSnapshot(ctx, st, ops.exists, d.Engine, anc); !live || err != nil {
		return Outcome{}, false, err
	}

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareRollbackStart,
		fmt.Sprintf("engine=%s ancestor=%s rollback_steps=%d", d.Engine, anc.TemplateName, steps),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": d.Engine, "source_db": source, "template": templateName,
			"fingerprint": key.Fingerprint(), "ancestor_template": anc.TemplateName,
			"ancestor_fingerprint": anc.Fingerprint, "rollback_steps": strconv.Itoa(steps),
		})

	rollbackFallback := func(reason string) {
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtPrepareRollbackFallback, reason,
			repoID, worktreeID, "", 0, map[string]string{
				"engine": d.Engine, "ancestor_fingerprint": anc.Fingerprint, "reason": reason,
			})
	}

	if err := ops.snapshotRestore(ctx, anc.TemplateName, source); err != nil {
		rollbackFallback(fmt.Sprintf("ancestor restore failed: %v", err))
		return Outcome{}, false, nil
	}

	// Rollback then re-migrate. Both run via runner.Run directly (not
	// runPhase) so a failure becomes a cold-build fall-through, not a
	// propagated error.
	rbSpec := runner.FromRollback(*d.Rollback, steps).
		WithLogPath(runnerLogPath(worktreePath, d.Engine, "rollback", source))
	rbStart := time.Now()
	out, err := runner.Run(ctx, rbSpec, worktreePath, source, tplCtx, inheritedEnv)
	if err != nil || out.ExitCode != 0 {
		rollbackFallback(runnerStepFailure("rollback", source, out, err))
		return Outcome{}, false, nil
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, source, "rollback", rbStart)

	mgSpec := runner.FromMigrate(*d.Migrate).
		WithLogPath(runnerLogPath(worktreePath, d.Engine, "migrate", source))
	mgStart := time.Now()
	out, err = runner.Run(ctx, mgSpec, worktreePath, source, tplCtx, inheritedEnv)
	if err != nil || out.ExitCode != 0 {
		rollbackFallback(runnerStepFailure("re-migrate", source, out, err))
		return Outcome{}, false, nil
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, source, "migrate", mgStart)

	if d.Seed != nil {
		sdSpec := runner.FromSeed(*d.Seed).
			WithLogPath(runnerLogPath(worktreePath, d.Engine, "seed", source))
		sdStart := time.Now()
		out, err = runner.Run(ctx, sdSpec, worktreePath, source, tplCtx, inheritedEnv)
		if err != nil || out.ExitCode != 0 {
			rollbackFallback(runnerStepFailure("seed", source, out, err))
			return Outcome{}, false, nil
		}
		emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, source, "seed", sdStart)
	}

	clones, err := snapshotRecordFanout(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, source, templateName, version, maxConns, key, inputs, ops)
	if err != nil {
		return Outcome{}, false, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
		fmt.Sprintf("rollback clones=%d duration=%dms ancestor=%s steps=%d",
			len(clones), ms, anc.TemplateName, steps),
		repoID, worktreeID, "", 0, map[string]string{
			"engine": d.Engine, "source_db": source, "template": templateName,
			"clones": strconv.Itoa(len(clones)), "cache_hit": "false",
			"rollback": "true", "ancestor_fingerprint": anc.Fingerprint,
			"rollback_steps": strconv.Itoa(steps), "duration_ms": strconv.FormatInt(ms, 10),
		})

	return Outcome{
		Engine: d.Engine, SourceDB: source, TemplateName: templateName,
		Fingerprint: key.Fingerprint(), CacheHit: false, Clones: clones,
		IncrementalBase: anc.Fingerprint,
	}, true, nil
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
) (out Outcome, err error) {
	started := time.Now()
	sourceDB, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}
	pgConn, err := cfg.Connections.ResolvePostgres(d.Connection)
	if err != nil {
		return Outcome{}, err
	}
	if pgConn == nil {
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, sourceDB,
			"connections.postgres not configured"), nil
	}

	drv, err := dbpostgres.Connect(ctx, *pgConn)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = drv.Close() }()

	// Probe keys ride the connection selector — different servers must
	// not share probe results (see prepareMySQL, #44).
	version, _ := cachedProbe(ctx, "postgres-version:"+d.Connection, func() (string, error) { return drv.EngineVersion(ctx) })
	maxConns, _ := cachedProbe(ctx, "postgres-maxconns:"+d.Connection, func() (int, error) { return drv.MaxConnections(ctx) })

	if d.BranchScoped {
		return runBranchScopedAt(ctx, version, branchScopedArgs{
			cfg: cfg, d: d, dbIdx: dbIdx, tplCtx: tplCtx, worktreePath: worktreePath,
			st: st, repoID: repoID, worktreeID: worktreeID, inheritedEnv: inheritedEnv,
			eng: &branchEngine{drv: postgresNS{drv}, scope: scopeName, engine: "postgres"},
			loadDump: func(ctx context.Context, active string, dump dumpFile) error {
				scoped, e := drv.OpenScoped(ctx, active)
				if e != nil {
					return e
				}
				defer func() { _ = scoped.Close() }()
				_, e = dumpload.LoadPostgres(ctx, scoped, pgConn, active, dump.Path)
				return e
			},
		})
	}

	key, inputs, err := computeKeyAndVectors(ctx, st, d, worktreePath, version)
	if err != nil {
		return Outcome{}, err
	}
	templateName := key.TemplateName()

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareStart,
		fmt.Sprintf("engine=postgres source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "postgres",
			"source_db":   sourceDB,
			"template":    templateName,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit? Fingerprint covers every declared input. With a spare
	// pool configured, restores (source + fanout clones alike) first try
	// to claim a pre-warmed spare via rename before paying a full
	// `CREATE DATABASE … TEMPLATE`.
	restore := restoreFor(d.Engine, engineconn.NewPostgresConn(drv), drv.SnapshotRestore,
		st, repoID, worktreeID, d)
	out, done, flight, err := cacheHitGeneric(
		ctx,
		drv.DatabaseExists,
		restore,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourceDB,
		key,
		maxConns,
		started,
	)
	defer flight.finish()
	if done || err != nil {
		return out, err
	}

	// Per-input vectors used for ancestor lookup AND persisted into
	// the new snapshot row so future preps can build incrementally
	// off this one. Mirrors prepareMySQL.

	ops := incrementalOps{
		exists:          drv.DatabaseExists,
		snapshotRestore: drv.SnapshotRestore,
		snapshotCreate:  drv.SnapshotCreate,
	}
	dumpKey := dumpOnlySnapshotKey(d.Engine, d.Connection, version, key.DumpHashHex)

	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, maxConns, key, inputs,
		inheritedEnv, started, ops)
	if done || err != nil {
		return out, err
	}

	out, done, err = tryRollbackIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, maxConns, key, inputs,
		inheritedEnv, started, ops)
	if done || err != nil {
		return out, err
	}

	out, done, err = tryDumpOnlyBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourceDB, templateName, version, maxConns, key, dumpKey,
		inputs, inheritedEnv, started, ops)
	if done || err != nil {
		return out, err
	}

	if err := postgresColdBuildSteps(
		ctx,
		drv,
		pgConn,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourceDB,
		version,
		dumpKey,
		inheritedEnv,
	); err != nil {
		return Outcome{}, err
	}
	clones, err := snapshotRecordFanout(ctx, cfg, d, tplCtx, worktreePath, st, repoID, worktreeID,
		sourceDB, templateName, version, maxConns, key, inputs, ops)
	if err != nil {
		return Outcome{}, err
	}
	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
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

// postgresColdBuildSteps runs the Postgres cold-build populate phase:
// drop + recreate the source DB, optionally load a dump (via a scoped
// connection), then run migrate and seed. Extracted verbatim from
// preparePostgres's cold-build block.
func postgresColdBuildSteps(
	ctx context.Context,
	drv *dbpostgres.Driver,
	pc *config.PostgresConn,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceDB, version string,
	dumpKey snapshot.Key,
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
			strategy, lerr := dumpload.LoadPostgres(ctx, scoped, pc, sourceDB, dr.Path)
			if lerr != nil {
				_ = scoped.Close()
				return fmt.Errorf("load dump %s: %w", dr.Path, lerr)
			}
			emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourceDB, dr.Path, i, len(dumps), stepStart, string(strategy))
		}
		_ = scoped.Close()
	}
	if err := seedDumpOnlyTemplate(
		ctx,
		st,
		d,
		repoID,
		worktreeID,
		sourceDB,
		version,
		dumpKey,
		len(dumps),
		drv.DatabaseExists,
		drv.SnapshotCreate,
	); err != nil {
		return err
	}
	if d.Migrate != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourceDB,
			"migrate",
			runner.FromMigrate(*d.Migrate),
			inheritedEnv,
		); err != nil {
			return err
		}
	}
	if d.Seed != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourceDB,
			"seed",
			runner.FromSeed(*d.Seed),
			inheritedEnv,
		); err != nil {
			return err
		}
	}
	return nil
}

// prepareMongo readies the per-worktree MongoDB database. Cold-build
// order matches the SQL engines: drop, optionally `mongorestore` a
// dump archive, run the seed step, snapshot the per-collection
// template, fan clones out. Cache-hit short-circuits straight to
// the per-collection $out clone.
//
//nolint:funlen // mirrors the linear cache-hit / incremental / cold-build / fanout flow used by every engine; extracting helpers just spreads the same conditions across functions
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
	mongoConn, err := cfg.Connections.ResolveMongodb(d.Connection)
	if err != nil {
		return Outcome{}, err
	}
	if mongoConn == nil {
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, sourceDB,
			"connections.mongodb not configured"), nil
	}
	drv, err := dbmongo.Connect(ctx, *mongoConn)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = drv.Close(ctx) }()

	version, _ := cachedProbe(ctx, "mongo-version:"+d.Connection, func() (string, error) { return drv.EngineVersion(ctx) })

	if d.BranchScoped {
		return runBranchScopedAt(ctx, version, branchScopedArgs{
			cfg: cfg, d: d, dbIdx: dbIdx, tplCtx: tplCtx, worktreePath: worktreePath,
			st: st, repoID: repoID, worktreeID: worktreeID, inheritedEnv: inheritedEnv,
			eng: &branchEngine{drv: mongoNS{drv}, scope: scopeName, engine: "mongodb"},
			loadDump: func(ctx context.Context, active string, dump dumpFile) error {
				// Per-entry SourceDB: each dump in the list can carry its own
				// `source_db:` for `--nsFrom=<source_db>.* --nsTo=<active>.*`.
				_, e := dbmongo.Restore(ctx, mongoConn, active, dump.SourceDB, dump.Path)
				return e
			},
		})
	}

	key, inputs, err := computeKeyAndVectors(ctx, st, d, worktreePath, version)
	if err != nil {
		return Outcome{}, err
	}
	templateName := key.TemplateName()

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareStart,
		fmt.Sprintf("engine=mongodb source=%s template=%s", sourceDB, templateName),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "mongodb",
			"source_db":   sourceDB,
			"template":    templateName,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit?
	out, done, flight, err := cacheHitGeneric(
		ctx,
		drv.DatabaseExists,
		drv.SnapshotRestore,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourceDB,
		key,
		0,
		started,
	)
	defer flight.finish()
	if done || err != nil {
		return out, err
	}

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

	if err := mongoColdBuildSteps(
		ctx,
		drv,
		mongoConn,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourceDB,
		inheritedEnv,
	); err != nil {
		return Outcome{}, err
	}
	snapStart := time.Now()
	if err := drv.SnapshotCreate(ctx, sourceDB, templateName); err != nil {
		return Outcome{}, fmt.Errorf("mongo snapshot create %s → %s: %w", sourceDB, templateName, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceDB, "snapshot-create", snapStart)
	if err := recordSnapshot(ctx, st, key, d.Engine, version, sourceDB, templateName, inputs, repoID); err != nil {
		return Outcome{}, err
	}
	spawnEvict(cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, drv.SnapshotRestore, templateName, clones, d.Engine, d.Fanout, 0); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
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

// mongoColdBuildSteps runs the MongoDB cold-build populate phase: drop
// the source DB, optionally mongorestore a dump archive, then run
// migrate and seed. Extracted verbatim from prepareMongo's cold-build
// block.
func mongoColdBuildSteps(
	ctx context.Context,
	drv *dbmongo.Driver,
	mc *config.MongoConn,
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
		strategy, rerr := dbmongo.Restore(ctx, mc, sourceDB, dr.SourceDB, dr.Path)
		if rerr != nil {
			return fmt.Errorf("mongo restore %s: %w", dr.Path, rerr)
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourceDB, dr.Path, i, len(dumps), stepStart, string(strategy))
	}
	if d.Migrate != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourceDB,
			"migrate",
			runner.FromMigrate(*d.Migrate),
			inheritedEnv,
		); err != nil {
			return err
		}
	}
	if d.Seed != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourceDB,
			"seed",
			runner.FromSeed(*d.Seed),
			inheritedEnv,
		); err != nil {
			return err
		}
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
	redisConn, err := cfg.Connections.ResolveRedis(d.Connection)
	if err != nil {
		return Outcome{}, err
	}
	if redisConn == nil {
		// No sourceDB to render here — KeyPrefix is the source identifier
		// for redis, and rendering it before the skip is harmless but
		// noisy in the event payload, so pass the raw prefix template.
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, d.KeyPrefix,
			"connections.redis not configured"), nil
	}
	if d.KeyPrefix == "" {
		return Outcome{}, errors.New("redis: key_prefix required")
	}
	drv, err := dbredis.Connect(ctx, *redisConn)
	if err != nil {
		return Outcome{}, err
	}
	defer func() { _ = drv.Close() }()

	if d.BranchScoped {
		others, err := siblingSlugs(ctx, st, repoID, worktreeID)
		if err != nil {
			return Outcome{}, err
		}
		return runBranchScoped(ctx, branchScopedArgs{
			cfg:          cfg,
			d:            d,
			dbIdx:        dbIdx,
			tplCtx:       tplCtx,
			worktreePath: worktreePath,
			st:           st,
			repoID:       repoID,
			worktreeID:   worktreeID,
			inheritedEnv: inheritedEnv,
			eng: &branchEngine{
				drv:    redisNS{d: drv, keep: siblingKeep(others)},
				scope:  scopePrefix,
				engine: "redis",
			},
		})
	}

	return prepareRedisPrefix(ctx, cfg, d, redisConn, drv, tplCtx, worktreePath, st, repoID, worktreeID, inheritedEnv)
}

// prepareRedisPrefix is the modern path — prefix-isolated keys in
// DB 0, fingerprint-cached template, parallel fanout.
func prepareRedisPrefix(
	ctx context.Context,
	cfg *config.Config,
	d config.DatabaseConfig,
	rc *config.RedisConn,
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
	version, _ := cachedProbe(ctx, "redis-version:"+d.Connection, func() (string, error) { return drv.EngineVersion(ctx) })
	key, inputs, err := computeKeyAndVectors(ctx, st, d, worktreePath, version)
	if err != nil {
		return Outcome{}, err
	}
	templatePrefix := "_tm:" + key.Fingerprint()[:16] + ":"

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareStart,
		fmt.Sprintf("engine=redis source=%s template=%s", sourcePrefix, templatePrefix),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "redis",
			"source_db":   sourcePrefix,
			"template":    templatePrefix,
			"fingerprint": key.Fingerprint(),
		})

	// Sibling-safe snapshot ops: under the main-wt overlay sourcePrefix is
	// bare and prefixes every sibling worktree's `<sourcePrefix><slug>_*`
	// keys. Filter the SOURCE of every capture and the TARGET drop of every
	// restore so cache-hit / incremental / cold-build never pull in or wipe
	// a sibling's keys. Template prefixes are hash-derived and never
	// collide, so existence probes stay unfiltered.
	rdOthers, err := siblingSlugs(ctx, st, repoID, worktreeID)
	if err != nil {
		return Outcome{}, err
	}
	rdKeep := func(k string) bool { return !nameOwnedByOtherSlug(k, rdOthers) }
	rdRestore := func(ctx context.Context, templatePrefix, target string) error {
		return drv.SnapshotRestoreFiltered(ctx, templatePrefix, target, rdKeep)
	}
	rdCreate := func(ctx context.Context, src, templatePrefix string) error {
		return drv.SnapshotCreateFiltered(ctx, src, templatePrefix, rdKeep)
	}

	// Cache hit?
	out, done, flight, err := cacheHitGeneric(
		ctx,
		drv.PrefixExists,
		rdRestore,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourcePrefix,
		key,
		0,
		started,
	)
	defer flight.finish()
	if done || err != nil {
		return out, err
	}

	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourcePrefix, templatePrefix, version, 0, key, inputs,
		inheritedEnv, started, incrementalOps{
			exists:          drv.PrefixExists,
			snapshotRestore: rdRestore,
			snapshotCreate:  rdCreate,
		})
	if done || err != nil {
		return out, err
	}

	// Cold build: drop source, run seed, snapshot template, fanout.
	if err := redisColdBuildSteps(ctx, drv, rc, d, tplCtx, worktreePath, st, repoID, worktreeID, sourcePrefix, inheritedEnv); err != nil {
		return Outcome{}, err
	}
	snapStart := time.Now()
	if err := rdCreate(ctx, sourcePrefix, templatePrefix); err != nil {
		return Outcome{}, fmt.Errorf("redis snapshot create %s → %s: %w", sourcePrefix, templatePrefix, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "snapshot-create", snapStart)
	if err := recordSnapshot(ctx, st, key, d.Engine, version, sourcePrefix, templatePrefix, inputs, repoID); err != nil {
		return Outcome{}, err
	}
	spawnEvict(cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, rdRestore, templatePrefix, clones, d.Engine, d.Fanout, 0); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
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

// redisColdBuildSteps runs the Redis-prefix cold-build populate phase:
// filtered drop of the source prefix, then migrate + seed. Extracted
// verbatim from prepareRedisPrefix's cold-build block.
func redisColdBuildSteps(
	ctx context.Context,
	drv *dbredis.Driver,
	rc *config.RedisConn,
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
	redisOthers, err := siblingSlugs(ctx, st, repoID, worktreeID)
	if err != nil {
		return err
	}
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
		strategy, lerr := drv.Restore(ctx, rc, sourcePrefix, dr.Path)
		if lerr != nil {
			return fmt.Errorf("load dump %s: %w", dr.Path, lerr)
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, dr.Path,
			i, len(dumps), stepStart, string(strategy))
	}
	if d.Migrate != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourcePrefix,
			"migrate",
			runner.FromMigrate(*d.Migrate),
			inheritedEnv,
		); err != nil {
			return err
		}
	}
	if d.Seed != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourcePrefix,
			"seed",
			runner.FromSeed(*d.Seed),
			inheritedEnv,
		); err != nil {
			return err
		}
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
//
//nolint:funlen // mirrors the linear cache-hit / incremental / cold-build / fanout flow used by every engine; extracting helpers just spreads the same conditions across functions
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
	esConnCfg, err := cfg.Connections.ResolveElasticsearch(d.Connection)
	if err != nil {
		return Outcome{}, err
	}
	if esConnCfg == nil {
		return emitPrepareSkipped(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix,
			"connections.elasticsearch not configured"), nil
	}
	drv, err := dbes.Connect(ctx, *esConnCfg)
	if err != nil {
		return Outcome{}, err
	}

	version, _ := cachedProbe(ctx, "es-version:"+d.Connection, func() (string, error) { return drv.EngineVersion(ctx) })
	if d.BranchScoped {
		others, err := siblingSlugs(ctx, st, repoID, worktreeID)
		if err != nil {
			return Outcome{}, err
		}
		return runBranchScopedAt(ctx, version, branchScopedArgs{
			cfg:          cfg,
			d:            d,
			dbIdx:        dbIdx,
			tplCtx:       tplCtx,
			worktreePath: worktreePath,
			st:           st,
			repoID:       repoID,
			worktreeID:   worktreeID,
			inheritedEnv: inheritedEnv,
			eng: &branchEngine{
				drv:    esNS{d: drv, keep: siblingKeep(others)},
				scope:  scopePrefix,
				engine: "elasticsearch",
			},
			loadDump: func(ctx context.Context, active string, dump dumpFile) error {
				return drv.Restore(ctx, active, dump.Path)
			},
		})
	}
	// Sibling-safe snapshot ops: under the main-wt overlay sourcePrefix is
	// bare and prefixes every sibling worktree's `<sourcePrefix>_<slug>_*`
	// indices. Filter the SOURCE of every capture and the TARGET drop of
	// every restore so cache-hit / incremental / cold-build never pull in
	// or wipe a sibling's data. Template prefixes are hash-derived and
	// never collide, so existence probes stay unfiltered.
	esOthers, err := siblingSlugs(ctx, st, repoID, worktreeID)
	if err != nil {
		return Outcome{}, err
	}
	esKeep := func(n string) bool { return !nameOwnedByOtherSlug(n, esOthers) }
	esRestore := func(ctx context.Context, templatePrefix, target string) error {
		return drv.SnapshotRestoreFiltered(ctx, templatePrefix, target, esKeep)
	}
	esCreate := func(ctx context.Context, src, templatePrefix string) error {
		return drv.SnapshotCreateFiltered(ctx, src, templatePrefix, esKeep)
	}

	key, inputs, err := computeKeyAndVectors(ctx, st, d, worktreePath, version)
	if err != nil {
		return Outcome{}, err
	}
	// ES forbids index names starting with `_`, so we use a
	// dedicated prefix (tm_<fingerprint>_) for the template indices.
	templatePrefix := key.IndexPrefix() + "_"

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareStart,
		fmt.Sprintf("engine=elasticsearch source=%s template=%s", sourcePrefix, templatePrefix),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      "elasticsearch",
			"source_db":   sourcePrefix,
			"template":    templatePrefix,
			"fingerprint": key.Fingerprint(),
		})

	// Cache hit? Verify by listing indices under the template prefix.
	// ES isolation is prefix-scoped: a cached template "exists" when ANY
	// index carries its prefix, so probe via ListMatching rather than the
	// exact-name IndexExists (which would always miss a prefix template
	// and force a needless cold rebuild on every cross-worktree reuse).
	esTemplateExists := func(ctx context.Context, prefix string) (bool, error) {
		matched, e := drv.ListMatching(ctx, prefix)
		return len(matched) > 0, e
	}
	out, done, flight, err := cacheHitGeneric(
		ctx,
		esTemplateExists,
		esRestore,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourcePrefix,
		key,
		0,
		started,
	)
	defer flight.finish()
	if done || err != nil {
		return out, err
	}

	out, done, err = tryIncrementalBuild(ctx, cfg, d, tplCtx, worktreePath, st,
		repoID, worktreeID, sourcePrefix, templatePrefix, version, 0, key, inputs,
		inheritedEnv, started, incrementalOps{
			exists: func(ctx context.Context, name string) (bool, error) {
				m, err := drv.ListMatching(ctx, name)
				return len(m) > 0, err
			},
			snapshotRestore: esRestore,
			snapshotCreate:  esCreate,
		})
	if done || err != nil {
		return out, err
	}

	if err := esColdBuildSteps(
		ctx,
		drv,
		esConnCfg,
		d,
		tplCtx,
		worktreePath,
		st,
		repoID,
		worktreeID,
		sourcePrefix,
		inheritedEnv,
	); err != nil {
		return Outcome{}, err
	}
	snapStart := time.Now()
	if err := esCreate(ctx, sourcePrefix, templatePrefix); err != nil {
		return Outcome{}, fmt.Errorf("es snapshot create %s → %s: %w", sourcePrefix, templatePrefix, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, "snapshot-create", snapStart)
	if err := recordSnapshot(ctx, st, key, d.Engine, version, sourcePrefix, templatePrefix, inputs, repoID); err != nil {
		return Outcome{}, err
	}
	spawnEvict(cfg, st, repoID)

	clones, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, esRestore, templatePrefix, clones, d.Engine, d.Fanout, 0); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
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

// prepareS3 prepares the per-worktree S3 bucket.
//
// Two modes, gated by `branch_scoped`:
//   - default (lifecycle-only): EnsureBucket(rendered key_prefix). The
//     rendered prefix IS the bucket name (bucket-per-worktree model).
//   - branch_scoped: the bucket's object set is swapped per branch via
//     the engine-agnostic durable-copy lifecycle (runBranchScoped) —
//     each branch keeps its own `tmbs-<hash>` durable bucket, captured
//     on switch-away and restored on switch-back with a server-side
//     whole-bucket copy. This is the S3 analogue of the mysql/postgres
//     branch-scoped durable database.
//
// `dbs3.ValidateBucketName` enforces AWS naming rules (3-63 chars,
// lowercase, dns-safe) on the rendered name before hitting the API so a
// misconfigured template surfaces with a clear treeman-side error, not a
// downstream `InvalidBucketName` from CreateBucket.
func prepareS3(
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
	s3Conn2, err := cfg.Connections.ResolveS3(d.Connection)
	if err != nil {
		return Outcome{}, err
	}
	if s3Conn2 == nil {
		return Outcome{}, errors.New("connections.s3 not configured")
	}
	if d.KeyPrefix == "" {
		return Outcome{}, errors.New("s3: key_prefix required (renders the bucket name)")
	}
	drv, err := dbs3.Connect(ctx, *s3Conn2)
	if err != nil {
		return Outcome{}, err
	}

	if d.BranchScoped {
		return runBranchScoped(ctx, branchScopedArgs{
			cfg:          cfg,
			d:            d,
			dbIdx:        dbIdx,
			tplCtx:       tplCtx,
			worktreePath: worktreePath,
			st:           st,
			repoID:       repoID,
			worktreeID:   worktreeID,
			inheritedEnv: inheritedEnv,
			eng: &branchEngine{
				drv:    s3NS{drv},
				scope:  scopePrefix,
				engine: "s3",
			},
		})
	}

	started := time.Now()
	bucket, err := template.Render(d.KeyPrefix, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render key_prefix: %w", err)
	}
	if err := dbs3.ValidateBucketName(bucket); err != nil {
		return Outcome{}, fmt.Errorf("s3: rendered key_prefix produced invalid %w (check the template in databases[].key_prefix)", err)
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareStart,
		"engine=s3 bucket="+bucket,
		repoID, worktreeID, "", 0, map[string]string{
			"engine":    "s3",
			"source_db": bucket,
		})
	if err := drv.EnsureBucket(ctx, bucket); err != nil {
		return Outcome{}, err
	}
	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
		fmt.Sprintf("s3 bucket=%s duration=%dms", bucket, ms),
		repoID, worktreeID, "", ms, map[string]string{
			"engine":      "s3",
			"source_db":   bucket,
			"duration_ms": strconv.FormatInt(ms, 10),
		})
	return Outcome{Engine: "s3", SourceDB: bucket}, nil
}

// prepareFileEngine is the file-backed family's prepare (engine
// "sqlite" / "duckdb"). The "database" is a regular file at the
// rendered `name_template` path (inside the worktree, or under
// connections.sqlite.base_dir), the template cache is a fingerprint-
// named sibling file, and every snapshot op is a reflink-preferring
// copy. No server, so there's no connection probe and no skip path —
// the family is always wired up.
func prepareFileEngine(
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
	rendered, err := template.Render(d.NameTemplate, tplCtx)
	if err != nil {
		return Outcome{}, fmt.Errorf("render name_template: %w", err)
	}
	sourceFile, err := dbfile.SourcePathFor(cfg, d.Connection, worktreePath, rendered)
	if err != nil {
		return Outcome{}, err
	}
	key, inputs, err := computeKeyAndVectors(ctx, st, d, worktreePath, "")
	if err != nil {
		return Outcome{}, err
	}
	templatePath := dbfile.TemplatePath(sourceFile, key.Fingerprint())

	// See prepareMySQL's pin comment — same race, same fix.
	unpinTemplate := snapshot.Pin(key.Fingerprint())
	defer unpinTemplate()

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareStart,
		fmt.Sprintf("engine=%s source=%s template=%s", d.Engine, sourceFile, templatePath),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      d.Engine,
			"source_db":   sourceFile,
			"template":    templatePath,
			"fingerprint": key.Fingerprint(),
		})

	// The shared cache-hit path resolves clone names with the bare
	// render (they're namespace names for the networked engines); the
	// file family needs them on the family base, so every name that
	// crosses the boundary gets resolved here.
	resolveFile := func(name string) (string, error) {
		if filepath.IsAbs(name) {
			return name, nil
		}
		return dbfile.SourcePathFor(cfg, d.Connection, worktreePath, name)
	}
	fileExists := func(_ context.Context, name string) (bool, error) {
		p, perr := resolveFile(name)
		if perr != nil {
			return false, perr
		}
		return dbfile.Exists(p)
	}
	fileRestore := func(_ context.Context, tmpl, target string) error {
		p, perr := resolveFile(target)
		if perr != nil {
			return perr
		}
		return dbfile.CopyDatabase(tmpl, p)
	}

	out, done, flight, err := cacheHitGeneric(ctx, fileExists, fileRestore, d, tplCtx, worktreePath,
		st, repoID, worktreeID, sourceFile, key, 0, started)
	defer flight.finish()
	if err != nil {
		return out, err
	}
	if done {
		for i, c := range out.Clones {
			p, perr := resolveFile(c)
			if perr != nil {
				return Outcome{}, fmt.Errorf("resolve clone %s: %w", c, perr)
			}
			out.Clones[i] = p
		}
		return out, nil
	}

	// Cold build: drop the family, seed the source file, snapshot it.
	if err := fileColdBuildSteps(ctx, d, tplCtx, worktreePath, st, repoID, worktreeID, sourceFile, templatePath, inheritedEnv); err != nil {
		return Outcome{}, err
	}
	snapStart := time.Now()
	if err := dbfile.CopyDatabase(sourceFile, templatePath); err != nil {
		return Outcome{}, fmt.Errorf("file snapshot create %s → %s: %w", sourceFile, templatePath, err)
	}
	emitPhaseDone(ctx, st, repoID, worktreeID, d.Engine, sourceFile, "snapshot-create", snapStart)
	if err := recordSnapshot(ctx, st, key, d.Engine, "", sourceFile, templatePath, inputs, repoID); err != nil {
		return Outcome{}, err
	}
	spawnEvict(cfg, st, repoID)

	clones, err := fileCloneNames(cfg, d, tplCtx, worktreePath)
	if err != nil {
		return Outcome{}, err
	}
	if err := fanOutClones(ctx, st, repoID, worktreeID, fileRestore, templatePath, clones, d.Engine, d.Fanout, 0); err != nil {
		return Outcome{}, err
	}

	ms := time.Since(started).Milliseconds()
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd,
		fmt.Sprintf("cold_build clones=%d duration=%dms", len(clones), ms),
		repoID, worktreeID, "", 0, map[string]string{
			"engine":      d.Engine,
			"source_db":   sourceFile,
			"template":    templatePath,
			"clones":      strconv.Itoa(len(clones)),
			"cache_hit":   "false",
			"duration_ms": strconv.FormatInt(ms, 10),
		})
	return Outcome{
		Engine:       d.Engine,
		SourceDB:     sourceFile,
		TemplateName: templatePath,
		Fingerprint:  key.Fingerprint(),
		CacheHit:     false,
		Clones:       clones,
	}, nil
}

// fileCloneNames resolves the test-clone fan-out names and joins them
// onto the family's base dir, exactly like the source path.
func fileCloneNames(cfg *config.Config, d config.DatabaseConfig, tplCtx template.Context, worktreePath string) ([]string, error) {
	rendered, err := resolveCloneNames(d.TestClones, tplCtx, worktreePath)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rendered))
	for i, r := range rendered {
		p, perr := dbfile.SourcePathFor(cfg, d.Connection, worktreePath, r)
		if perr != nil {
			return nil, perr
		}
		out[i] = p
	}
	return out, nil
}

// fileColdBuildSteps rebuilds the source database file from scratch:
// drop the whole family (source + template + clones), then seed —
// binary dump files are copied in as the base database, .sql dumps
// execute against it in list order (a .sql dump with no preceding
// binary seeds an empty sqlite database) — then migrate + seed runner
// phases, with {target_db} resolving to the file path.
func fileColdBuildSteps(
	ctx context.Context,
	d config.DatabaseConfig,
	tplCtx template.Context,
	worktreePath string,
	st *store.Store,
	repoID, worktreeID int64,
	sourceFile, templatePath string,
	inheritedEnv map[string]string,
) error {
	dropped, err := dbfile.RemoveDatabase(sourceFile)
	if err != nil {
		return err
	}
	if t, terr := dbfile.RemoveDatabase(templatePath); terr == nil {
		dropped = append(dropped, t...)
	}
	if cfgLikeClones, cerr := dbfile.CloneSiblings(filepath.Dir(sourceFile), filepath.Base(sourceFile)); cerr == nil {
		for _, s := range cfgLikeClones {
			if r, rerr := dbfile.RemoveDatabase(s); rerr == nil {
				dropped = append(dropped, r...)
			}
		}
	}
	emitColdBuildDrop(ctx, st, repoID, worktreeID, d.Engine,
		fmt.Sprintf("%s (%d files)", sourceFile, len(dropped)))
	dumps, err := dumpsReady(d.Dump, worktreePath)
	if err != nil {
		return err
	}
	for i, dr := range dumps {
		stepStart := time.Now()
		var strategy string
		if strings.HasSuffix(strings.ToLower(dr.Path), ".sql") {
			if err := dbfile.ApplySQLDump(ctx, sourceFile, dr.Path); err != nil {
				return fmt.Errorf("load dump %s: %w", dr.Path, err)
			}
			strategy = "sql-apply"
		} else {
			if err := dbfile.CopyDatabase(dr.Path, sourceFile); err != nil {
				return fmt.Errorf("load dump %s: %w", dr.Path, err)
			}
			strategy = "file-copy"
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourceFile, dr.Path,
			i, len(dumps), stepStart, strategy)
	}
	if d.Migrate != nil {
		if err := runPhase(ctx, st, repoID, worktreeID, d, tplCtx, worktreePath,
			sourceFile, "migrate", runner.FromMigrate(*d.Migrate), inheritedEnv); err != nil {
			return err
		}
	}
	if d.Seed != nil {
		if err := runPhase(ctx, st, repoID, worktreeID, d, tplCtx, worktreePath,
			sourceFile, "seed", runner.FromSeed(*d.Seed), inheritedEnv); err != nil {
			return err
		}
	}
	return nil
}

// esColdBuildSteps runs the Elasticsearch cold-build populate phase:
// filtered drop of the source-prefix indices, optionally bulk-load a
// dump NDJSON, then run migrate and seed. Extracted verbatim from
// prepareES's cold-build block.
func esColdBuildSteps(
	ctx context.Context,
	drv *dbes.Driver,
	ec *config.EsConn,
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
	others, err := siblingSlugs(ctx, st, repoID, worktreeID)
	if err != nil {
		return err
	}
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
		strategy, rerr := drv.DispatchRestore(ctx, ec, sourcePrefix, dr.Path)
		if rerr != nil {
			return fmt.Errorf("es restore %s: %w", dr.Path, rerr)
		}
		emitDumpLoadPhase(ctx, st, repoID, worktreeID, d.Engine, sourcePrefix, dr.Path,
			i, len(dumps), stepStart, string(strategy))
	}
	if d.Migrate != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourcePrefix,
			"migrate",
			runner.FromMigrate(*d.Migrate),
			inheritedEnv,
		); err != nil {
			return err
		}
	}
	if d.Seed != nil {
		if err := runPhase(
			ctx,
			st,
			repoID,
			worktreeID,
			d,
			tplCtx,
			worktreePath,
			sourcePrefix,
			"seed",
			runner.FromSeed(*d.Seed),
			inheritedEnv,
		); err != nil {
			return err
		}
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
// inputWalk is one databases[].inputs[] glob's expansion: the matched
// absolute paths plus their per-file content hashes, from one batched
// stat-gated cache round-trip. The snapshot key's per-glob folds and
// the ancestor vectors both derive from it, so the glob walk runs once
// per prepare, not once per consumer.
type inputWalk struct {
	glob      string
	hashByAbs map[string]string
}

func walkInputs(ctx context.Context, st *store.Store, d config.DatabaseConfig, worktreePath string) ([]inputWalk, error) {
	if st == nil {
		return nil, nil
	}
	var walks []inputWalk
	for _, in := range d.Inputs {
		matches, err := doublestar.FilepathGlob(filepath.Join(worktreePath, in.Glob))
		if err != nil {
			return nil, fmt.Errorf("expand input glob %q: %w", in.Glob, err)
		}
		if len(matches) == 0 {
			continue
		}
		hashByAbs, err := st.BatchHashedFiles(ctx, matches)
		if err != nil {
			return nil, fmt.Errorf("hash the %d files of input glob %q: %w", len(matches), in.Glob, err)
		}
		walks = append(walks, inputWalk{glob: in.Glob, hashByAbs: hashByAbs})
	}
	return walks, nil
}

// computeKeyAndVectors is the single-walk form of computeSnapshotKey +
// computeInputVectors: one expansion + one BatchHashedFiles round-trip
// per glob feeds both the snapshot key and the ancestor vectors (the
// two previously re-walked and re-hashed the exact same files).
func computeKeyAndVectors(
	ctx context.Context,
	st *store.Store,
	d config.DatabaseConfig,
	worktreePath, engineVersion string,
) (snapshot.Key, map[string]store.InputVector, error) {
	walks, err := walkInputs(ctx, st, d, worktreePath)
	if err != nil {
		return snapshot.Key{}, nil, err
	}
	key, err := keyFromWalks(ctx, st, walks, d, worktreePath, engineVersion)
	if err != nil {
		return snapshot.Key{}, nil, err
	}
	return key, vectorsFromWalks(worktreePath, walks), nil
}

func computeSnapshotKey(
	ctx context.Context,
	st *store.Store,
	d config.DatabaseConfig,
	worktreePath, engineVersion string,
) (snapshot.Key, error) {
	walks, err := walkInputs(ctx, st, d, worktreePath)
	if err != nil {
		return snapshot.Key{}, err
	}
	return keyFromWalks(ctx, st, walks, d, worktreePath, engineVersion)
}

// keyFromWalks builds the canonical Key from a completed input walk.
func keyFromWalks(
	ctx context.Context,
	st *store.Store,
	walks []inputWalk,
	d config.DatabaseConfig,
	worktreePath, engineVersion string,
) (snapshot.Key, error) {
	// 1. Fold each glob's per-file hashes into one entry, keyed by the
	//    glob so the user can tell from the SQLite row what changed.
	//    Files are keyed by worktree-relative path: unique within the
	//    glob, and identical across worktrees so templates stay shared.
	inputHashes := map[string]string{}
	for _, w := range walks {
		relHash := make(map[string]string, len(w.hashByAbs))
		for abs, h := range w.hashByAbs {
			rel, err := filepath.Rel(worktreePath, abs)
			if err != nil {
				rel = abs
			}
			relHash[filepath.ToSlash(rel)] = h
		}
		var sb strings.Builder
		for _, k := range sortedKeys(relHash) {
			sb.WriteString(k + ":" + relHash[k] + "\n")
		}
		inputHashes[w.glob] = sb.String()
	}

	// 2. Hash every dump file in declared ORDER and fold them into a
	//    single DumpHashHex. Order matters: two dumps in different
	//    sequence can produce different DB state. A single dump's hash
	//    is the file's own hash.
	dumpHash := ""
	if len(d.Dump) > 0 {
		absPaths := make([]string, 0, len(d.Dump))
		for _, dn := range d.Dump {
			absPaths = append(absPaths, filepath.Join(worktreePath, dn.Path))
		}
		hashes, err := snapshot.LockfileHashesForWithCache(ctx, st, absPaths)
		if err != nil {
			return snapshot.Key{}, fmt.Errorf("hash the dump files of %s: %w", d.Engine, err)
		}
		if len(d.Dump) == 1 {
			dumpHash = hashes[absPaths[0]]
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
				_, _ = h.Write([]byte(hashes[absPaths[i]]))
				_, _ = h.Write([]byte{0})
			}
			dumpHash = hex.EncodeToString(h.Sum(nil))
		}
	}

	// 3. Hash the migrate + seed run-strings + env maps. Changing
	// what the user told us to run is itself an input change.
	cmdHash := commandsHash(d)

	// Stash command hash + dump hash in lockfileHashes (the snapshot
	// Key already has a string-map field for ancillary inputs).
	if cmdHash != "" {
		inputHashes["__commands__"] = cmdHash
	}
	return snapshot.New(d.Engine, engineVersion, d.Connection, "", "", dumpHash, inputHashes), nil
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
	Error             string                `json:"error,omitempty"`
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
	key, err := computeSnapshotKey(ctx, st, d, worktreePath, engineVersion)
	if err != nil {
		return FingerprintReport{Engine: d.Engine, EngineVersion: engineVersion, SourceDB: sourceDB, Error: err.Error()}
	}
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
		rec, err := st.LookupSnapshot(ctx, key.Fingerprint())
		if err != nil {
			rep.Error = fmt.Sprintf("look up the cached snapshot %s: %v", key.Fingerprint(), err)
			return rep
		}
		if rec != nil {
			rep.CachedSnapshot = rec
			rep.CacheHitAvailable = true
		}
	}
	return rep
}

// vectorsFromWalks renders the ancestor vectors (the per-input ordered
// file vectors stored alongside a snapshot so FindAncestorSnapshot can
// detect a content-prefix ancestor) from a completed input walk: each
// `databases[].inputs[]` glob contributes one vector of repo-relative
// paths with per-file content hashes (keyed by absolute path so
// duplicate basenames in different subdirs don't collide). Empty or
// missing globs contribute no entry.
func vectorsFromWalks(worktreePath string, walks []inputWalk) map[string]store.InputVector {
	out := map[string]store.InputVector{}
	for _, w := range walks {
		vec := make(store.InputVector, 0, len(w.hashByAbs))
		for abs, h := range w.hashByAbs {
			rel, relErr := filepath.Rel(worktreePath, abs)
			if relErr != nil {
				rel = filepath.Base(abs)
			}
			vec = append(vec, store.FileHash{Path: rel, Hash: h})
		}
		sort.Slice(vec, func(i, j int) bool { return vec[i].Path < vec[j].Path })
		out[w.glob] = vec
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
	// Rollback is deliberately NOT folded in: it affects only the build
	// *transformation* (how an edited template is rebuilt), never the
	// template's content or which migrations `migrate` applies. Folding
	// it would force needless rebuilds on a rollback-config change and,
	// worse, break rollback-ancestor matching the first time a user adds
	// a `rollback:` block (every prior template would carry a different
	// commands hash). Templates built with the same migrate+seed are
	// valid rollback ancestors regardless of rollback config.
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
// cfg.Databases and returns every engine's failure, joined in config
// order. One engine failing doesn't stop the others: a missing redis
// must not block dropping mysql.
//
// Engines tear down in parallel: each one drives a different driver
// (mysql + mongo + redis + ES) so there's no shared connection pool
// to contend on, and a slow ES delete-by-prefix shouldn't gate the
// fast mysql DROP DATABASE.
//
// With a worktree id, the outcome is recorded on the worktree row. A
// failure is kept as its teardown_error, and the daemon retries it
// after the row is marked deleted. The worktree's active-branch
// markers stay, so the retry still captures each branch_scoped
// namespace into its durable before dropping it. A success clears the
// error and every marker, including markers for databases since
// removed from config, so a worktree re-created at the same path
// starts clean.
func TeardownDatabases(
	ctx context.Context,
	cfg *config.Config,
	sl string,
	repoID, worktreeID int64,
	st *store.Store,
) error {
	tplCtx := template.FromSlug(slug.Slug{Value: sl, Source: slug.SourceTicket})

	errs := make([]error, len(cfg.Databases))
	var wg sync.WaitGroup
	for i, d := range cfg.Databases {
		wg.Go(func() {
			err := teardownOne(ctx, cfg, d, tplCtx, sl, repoID, worktreeID, st)
			if err == nil {
				return
			}
			_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtDBTeardownError,
				err.Error(), repoID, worktreeID, "", 0, nil)
			errs[i] = fmt.Errorf("teardown %s database %d (slug %s): %w", d.Engine, i, sl, err)
		})
	}
	wg.Wait()
	err := errors.Join(errs...)
	if worktreeID <= 0 {
		return err
	}
	return errors.Join(err, recordTeardownOutcome(ctx, st, worktreeID, err))
}

// recordTeardownOutcome stores a worktree's teardown result: the error
// to retry, or on success no pending error and no active-branch markers.
func recordTeardownOutcome(ctx context.Context, st *store.Store, worktreeID int64, teardownErr error) error {
	if teardownErr != nil {
		if err := st.SetTeardownError(ctx, worktreeID, teardownErr.Error()); err != nil {
			return fmt.Errorf("record teardown error for worktree %d: %w", worktreeID, err)
		}
		return nil
	}
	if err := st.ClearActiveBranchesForWorktree(ctx, worktreeID); err != nil {
		return fmt.Errorf("clear active-branch markers for worktree %d: %w", worktreeID, err)
	}
	if err := st.ClearTeardownError(ctx, worktreeID); err != nil {
		return fmt.Errorf("clear teardown error for worktree %d: %w", worktreeID, err)
	}
	return nil
}

// connPool holds one live connection per (engine family, connection
// name) for batch teardowns: dialing per database means N x engines
// full connect + ping + container-resolve handshakes, which dominates
// a main-wt purge over long branch history (#88). Named blocks dial
// separately from the singular default (#44).
type connPool map[teardownConnKey]engineconn.Conn

type teardownConnKey struct {
	fam  engine.Family
	name string
}

func (p connPool) get(
	ctx context.Context,
	cfg *config.Config,
	fam engine.Family,
	name string,
	connect func(context.Context, *config.Config, engine.Family, string) (engineconn.Conn, bool, error),
) (engineconn.Conn, error) {
	key := teardownConnKey{fam, name}
	if c, ok := p[key]; ok {
		return c, nil
	}
	conn, _, err := connect(ctx, cfg, fam, name)
	if err != nil {
		return nil, err
	}
	p[key] = conn
	return conn, nil
}

func (p connPool) closeAll() {
	for _, c := range p {
		_ = c.Close()
	}
}

// TeardownSlugsConnReuse tears down every slug's namespaces for all
// configured databases, reusing ONE connection per engine family
// across the whole batch: #configured engines connects total instead
// of len(slugs) x #engines. Event shapes match the single-slug
// TeardownDatabases path; per-slug errors are warned + skipped (a bad
// branch must not strand the rest) and the returned count is how many
// slugs tore down without any family-level error.
func TeardownSlugsConnReuse(
	ctx context.Context,
	cfg *config.Config,
	slugs []string,
	repoID, worktreeID int64,
	st *store.Store,
) int {
	return teardownSlugsVia(ctx, cfg, slugs, repoID, worktreeID, st, engineconn.Connect)
}

// teardownSlugsVia is the testable core of TeardownSlugsConnReuse —
// `connect` is injectable so the connection-reuse count is assertable
// without a live engine.
func teardownSlugsVia(
	ctx context.Context,
	cfg *config.Config,
	slugs []string,
	repoID, worktreeID int64,
	st *store.Store,
	connect func(context.Context, *config.Config, engine.Family, string) (engineconn.Conn, bool, error),
) int {
	pool := connPool{}
	defer pool.closeAll()

	purged := 0
	for _, sl := range slugs {
		tplCtx := template.FromSlug(slug.Slug{Value: sl, Source: slug.SourceTicket})
		failed := false
		for _, d := range cfg.Databases {
			// Branch-scoped databases tear down through the swap layer,
			// which owns its connection lifecycle — fall back to it.
			if d.BranchScoped {
				if err := teardownBranchScoped(ctx, cfg, d, repoID, worktreeID, st); err != nil {
					slog.Warn("batch teardown", "slug", sl, "engine", d.Engine, "err", err)
					failed = true
				}
				continue
			}
			fam, ok := engine.Canonical(d.Engine)
			if !ok || !engineconn.Configured(cfg, fam, d.Connection) {
				continue
			}
			target, err := teardownTarget(ctx, cfg, st, worktreeID, d, fam, tplCtx)
			if err != nil {
				slog.Warn("batch teardown", "slug", sl, "engine", d.Engine, "err", err)
				failed = true
				continue
			}
			conn, err := pool.get(ctx, cfg, fam, d.Connection, connect)
			if err != nil {
				slog.Warn("batch teardown connect", "engine", string(fam), "err", err)
				failed = true
				continue
			}
			drop := func(ctx context.Context, t string) (int, error) {
				return dropOwned(ctx, conn, fam, t, d.TestClones, tplCtx)
			}
			if err := teardownGeneric(ctx, string(fam), target, sl, repoID, worktreeID, st, drop); err != nil {
				slog.Warn("batch teardown", "slug", sl, "engine", d.Engine, "err", err)
				failed = true
			}
		}
		if !failed {
			purged++
		}
	}
	return purged
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
	fam, ok := engine.Canonical(d.Engine)
	if !ok {
		// Unknown engine — log + event so a typo'd `engine: postgress`
		// doesn't turn worktree teardown into a silent no-op (same
		// shape as the cold-build sibling-wipe that originally hid in
		// the event log).
		_ = st.WriteEvent(ctx, store.LevelWarn, store.EvtDBTeardownSkip,
			fmt.Sprintf("teardown skipped: unknown engine %q (allowed: %s)", d.Engine, engine.KnownList()),
			repoID, worktreeID, "", 0, map[string]any{
				"engine": d.Engine, "slug": sl,
			})
		return nil
	}
	// Engine isn't wired up — nothing was ever created. Silent skip,
	// matching the prepare side.
	if !engineconn.Configured(cfg, fam, d.Connection) {
		return nil
	}
	target, err := teardownTarget(ctx, cfg, st, worktreeID, d, fam, tplCtx)
	if err != nil {
		return err
	}
	conn, _, err := engineconn.Connect(ctx, cfg, fam, d.Connection)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return teardownGeneric(ctx, string(fam), target, sl, repoID, worktreeID, st,
		func(ctx context.Context, t string) (int, error) {
			return dropOwned(ctx, conn, fam, t, d.TestClones, tplCtx)
		})
}

// fileTeardownTarget resolves a rendered file-family name_template
// onto the family's base directory (worktree root or
// connections.sqlite.base_dir — including per-connection base dirs),
// using the worktree row's path: teardown runs before the row goes
// away.
func fileTeardownTarget(
	ctx context.Context,
	cfg *config.Config,
	st *store.Store,
	worktreeID int64,
	connName, rendered string,
) (string, error) {
	wtPath, err := st.WorktreePathByID(ctx, worktreeID)
	if err != nil {
		return "", err
	}
	return dbfile.SourcePathFor(cfg, connName, wtPath, rendered)
}

// teardownGeneric drops the already-rendered per-worktree `target`
// (database name or key/index prefix) via `drop` and writes the `db_drop`
// event — the shared tail of teardownOne, kept separate so the event
// shape lives in one place.
func teardownGeneric(
	ctx context.Context,
	engineLabel, target, sl string,
	repoID, worktreeID int64,
	st *store.Store,
	drop func(context.Context, string) (int, error),
) error {
	count, err := drop(ctx, target)
	if err != nil {
		return err
	}
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtDBDrop,
		fmt.Sprintf("%s: %s (%d)", engineLabel, target, count),
		repoID, worktreeID, "", 0, map[string]any{
			"engine": engineLabel, "slug": sl, "target": target, "count": count,
		})
	return nil
}
