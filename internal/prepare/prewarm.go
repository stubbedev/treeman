package prepare

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/engineconn"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
)

// spareEngine bundles the spare-pool capabilities of one engine's
// already-connected driver (#53): replenishment clones via
// SnapshotCreator, cache-hit claims via SpareClaimer (Postgres rename,
// MySQL staged physical clone). Built from any engineconn.Conn whose
// driver implements the capabilities.
type spareEngine struct {
	engine  string
	conn    engineconn.Conn
	creator engineconn.SnapshotCreator
	claimer engineconn.SpareClaimer
}

// spareEngineFromConn resolves the capability set from `conn`; nil
// when it lacks the SnapshotCreator floor. The claimer is optional:
// without it the pool replenishes but restores can't claim (a future
// engine can ship create-only).
func spareEngineFromConn(engineName string, conn engineconn.Conn) *spareEngine {
	creator, ok := conn.(engineconn.SnapshotCreator)
	if !ok {
		return nil
	}
	claimer, _ := conn.(engineconn.SpareClaimer)
	return &spareEngine{engine: engineName, conn: conn, creator: creator, claimer: claimer}
}

// spareEngineFor dials `engineName` fresh and resolves its spare
// capabilities — the path used by the detached replenisher. (nil, err)
// when unconfigured or incapable.
func spareEngineFor(ctx context.Context, cfg *config.Config, engineName string) (*spareEngine, error) {
	fam, ok := engine.Canonical(engineName)
	if !ok {
		return nil, fmt.Errorf("prewarm: unsupported engine %q", engineName)
	}
	// The spare pool is a singular-connection concept today; named
	// postgres blocks don't participate.
	conn, configured, err := engineconn.Connect(ctx, cfg, fam, "")
	if !configured {
		return nil, fmt.Errorf("prewarm: connections.%s not configured", fam)
	}
	if err != nil {
		return nil, fmt.Errorf("prewarm: connect %s: %w", fam, err)
	}
	se := spareEngineFromConn(string(fam), conn)
	if se == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("prewarm: engine %s cannot create spare snapshots", fam)
	}
	return se, nil
}

// spareClaimRestore wraps the plain restore with the spare-claim fast
// path: try to claim one of the template's pre-warmed spares, which
// replaces the target in place, so it never stops existing. A claim is
// milliseconds (Postgres rename) or a file-copy import (MySQL physical
// clone) versus a full logical restore. The restorer serves the
// cache-hit source AND its fanout clones concurrently, so slots are
// handed out by one shared allocator: each slot is tried by exactly one
// restore — no two claims contend for the same spare (Postgres: the
// rename race; MySQL, where a claim copies rather than consumes the
// spare: the shared export lock) — and a pool of N covers at most the
// first N restores of a prepare. A restore whose slots are exhausted or
// unclaimable falls back to the plain restore.
func spareClaimRestore(
	se *spareEngine,
	st *store.Store,
	repoID, worktreeID int64,
	prewarm uint32,
	plainRestore func(ctx context.Context, template, target string) error,
) cloneRestorer {
	var nextSlot atomic.Uint32
	return func(ctx context.Context, template, target string) error {
		if se.claimer == nil {
			return plainRestore(ctx, template, target)
		}
		for slot := nextSlot.Add(1); slot <= prewarm; slot = nextSlot.Add(1) {
			spare := snapshot.SpareName(template, int(slot))
			if err := se.claimer.ClaimSpare(ctx, spare, target); err == nil {
				_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsPrewarmClaim,
					fmt.Sprintf("claimed spare %s → %s", spare, target),
					repoID, worktreeID, "", 0, map[string]string{
						"engine":   se.engine,
						"template": template,
						"spare":    spare,
						"target":   target,
					})
				return nil
			}
		}
		return plainRestore(ctx, template, target)
	}
}

// restoreFor picks the restore strategy for one database: plain
// SnapshotRestore, or the spare-claim wrapper when a pre-warm pool is
// configured and the engine has the capabilities. Works for any
// capability-bearing family — postgres and mysql today (#53).
func restoreFor(
	engineName string,
	conn engineconn.Conn,
	plainRestore func(ctx context.Context, template, target string) error,
	st *store.Store,
	repoID, worktreeID int64,
	d config.DatabaseConfig,
) cloneRestorer {
	if d.Prewarm == 0 {
		return plainRestore
	}
	se := spareEngineFromConn(engineName, conn)
	if se == nil {
		slog.Warn("prewarm configured but engine lacks spare capabilities",
			"engine", engineName, "template_pool", d.Prewarm)
		return plainRestore
	}
	return spareClaimRestore(se, st, repoID, worktreeID, d.Prewarm, plainRestore)
}

// maybeSpawnPrewarm is the deferred pool top-up, run by
// prepareOneEngine on the Outcome a family's prepare path RETURNED —
// cache hit (spares were claimed), incremental/rollback/dump-only and
// cold builds (fresh template, empty pool) all return the template
// they left in place. Deciding from the returned value rather than a
// per-engine defer over locals keeps every family on one contract: a
// defer in prepareMySQL once read the cache-miss locals instead of the
// cold build's result and never topped the pool up. Skipped outcomes
// carry no template; branch_scoped and non-capable engines are
// rejected with prewarm by config validation.
func maybeSpawnPrewarm(
	cfg *config.Config,
	st *store.Store,
	repoID, worktreeID int64,
	d config.DatabaseConfig,
	out Outcome,
	err error,
) {
	if err != nil || d.Prewarm == 0 || out.TemplateName == "" || out.Fingerprint == "" {
		return
	}
	spawnPrewarm(cfg, st, repoID, worktreeID, d.Engine, out.Fingerprint, out.TemplateName, d.Prewarm)
}

// prewarmInFlight dedups concurrent replenishers per fingerprint —
// two finalizes hitting the same template would otherwise both walk
// the slot list and double-restore the same spares.
var prewarmInFlight sync.Map

// spawnPrewarm tops the template's spare pool back up to n slots in a
// detached goroutine, mirroring spawnEvict's pattern: fresh background
// context (the prepare that triggered it must not block on, nor cancel,
// pool maintenance), hard 5-minute timeout so a stalled CREATE can't
// wedge the goroutine, and safego panic recovery. The fingerprint is
// pinned for the duration so a concurrent GC sweep can't drop the
// template out from under a spare mid-clone.
//
// Engine-generic (#53): the connection + spare capabilities resolve
// from the registry via spareEngineFor, so mysql pools replenish the
// same way postgres ones do.
//
// Slot-name idempotence makes replenish self-healing: only missing
// `_spare<i>` slots are created, and slots beyond n (config shrank)
// are reaped best-effort.
func spawnPrewarm(
	cfg *config.Config,
	st *store.Store,
	repoID, worktreeID int64,
	engineName string,
	fingerprint, templateName string,
	n uint32,
) {
	if n == 0 {
		return
	}
	if st == nil {
		return
	}
	if _, busy := prewarmInFlight.LoadOrStore(fingerprint, struct{}{}); busy {
		return
	}
	st.Go("snapshot:prewarm", templateName, func() {
		defer prewarmInFlight.Delete(fingerprint)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		unpin := snapshot.Pin(fingerprint)
		defer unpin()

		se, err := spareEngineFor(ctx, cfg, engineName)
		if err != nil {
			slog.Warn("prewarm resolve engine", "engine", engineName, "template", templateName, "err", err)
			return
		}
		defer func() { _ = se.conn.Close() }()

		// The template can vanish between the triggering prepare and
		// this goroutine running (eviction raced the pin). Spares of a
		// dead template are unreachable, so just bail.
		createdN := replenishSpares(ctx, se, templateName, n)

		if createdN > 0 {
			_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsPrewarm,
				fmt.Sprintf("pre-warmed %d spare(s) for %s", createdN, templateName),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":   se.engine,
					"template": templateName,
					"created":  strconv.Itoa(createdN),
					"pool":     strconv.Itoa(int(n)),
				})
		}
	})
}

func replenishSpares(ctx context.Context, se *spareEngine, templateName string, n uint32) int {
	alive, err := se.conn.Exists(ctx, templateName)
	if err != nil {
		slog.Warn("prewarm probe template", "engine", se.engine, "template", templateName, "err", err)
		return 0
	}
	if !alive {
		return 0
	}

	created := atomic.Int64{}
	// Missing slots are restored concurrently: each restore is a full
	// template copy, so serial top-up after a burst made the tail of
	// the burst wait n x copy (the exact cost the pool exists to
	// avoid). Slot names are distinct so restores can't collide; the
	// cap keeps us from piling copy load onto the server beyond a
	// small multiple of what a create burst needs.
	restoreLimit := int(min(n, 4))
	var g errgroup.Group
	g.SetLimit(restoreLimit)
	for slot := 1; slot <= int(n); slot++ {
		name := snapshot.SpareName(templateName, slot)
		exists, err := se.conn.Exists(ctx, name)
		if err != nil {
			slog.Warn("prewarm probe spare", "spare", name, "template", templateName, "err", err)
			continue
		}
		if exists {
			continue
		}
		g.Go(func() error {
			if err := se.creator.CreateSnapshot(ctx, templateName, name); err != nil {
				slog.Warn("prewarm restore", "spare", name, "template", templateName, "err", err)
				return err
			}
			created.Add(1)
			return nil
		})
	}
	_ = g.Wait()

	// Reap slots beyond n so shrinking `prewarm` in config actually
	// shrinks the pool instead of leaving zombie spares around until
	// template eviction.
	names, err := se.conn.ListMatching(ctx, templateName+snapshot.PrewarmSuffix)
	if err != nil {
		slog.Warn("prewarm list spares to reap", "template", templateName, "err", err)
	}
	for _, name := range names {
		if slot, ok := snapshot.SpareSlot(name, templateName); ok && slot > int(n) {
			if err := se.conn.DropSnapshot(ctx, name); err != nil {
				slog.Warn("prewarm reap extra slot", "spare", name, "err", err)
			}
		}
	}
	return int(created.Load())
}
