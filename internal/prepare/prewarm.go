package prepare

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	"github.com/stubbedev/treeman/internal/safego"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
)

// postgresClaimRestore wraps drv.SnapshotRestore with the spare-claim
// fast path: drop the target, then try to RENAME one of the template's
// pre-warmed spares onto it — a catalog-only operation that's
// milliseconds regardless of database size, versus the block copy
// `CREATE DATABASE … TEMPLATE` pays. Rename is atomic (Postgres
// refuses to overwrite), so two concurrent claimers of the same slot
// resolve cleanly: the loser tries the next slot, and when the pool is
// dry the wrapper falls back to a plain restore. The same restorer
// serves the cache-hit source AND its fanout clones, so a pool of N
// covers the first N restores of a worktree create.
func postgresClaimRestore(
	drv *dbpostgres.Driver,
	st *store.Store,
	repoID, worktreeID int64,
	prewarm uint32,
) cloneRestorer {
	return func(ctx context.Context, template, target string) error {
		// Rename can't overwrite, so clear the target first. A failed
		// drop (e.g. lingering connections) just means no fast path —
		// SnapshotRestore re-attempts the drop with its own semantics.
		if err := drv.DropDatabase(ctx, target); err == nil {
			for slot := 1; slot <= int(prewarm); slot++ {
				spare := snapshot.SpareName(template, slot)
				if err := drv.RenameDatabase(ctx, spare, target); err == nil {
					_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsPrewarmClaim,
						fmt.Sprintf("claimed spare %s → %s", spare, target),
						repoID, worktreeID, "", 0, map[string]string{
							"engine":   "postgres",
							"template": template,
							"spare":    spare,
							"target":   target,
						})
					return nil
				}
			}
		}
		return drv.SnapshotRestore(ctx, template, target)
	}
}

// postgresRestoreFor picks the restore strategy for one database:
// plain SnapshotRestore, or the spare-claim wrapper when a pre-warm
// pool is configured.
func postgresRestoreFor(
	drv *dbpostgres.Driver,
	st *store.Store,
	repoID, worktreeID int64,
	d config.DatabaseConfig,
) cloneRestorer {
	if d.Prewarm == 0 {
		return drv.SnapshotRestore
	}
	return postgresClaimRestore(drv, st, repoID, worktreeID, d.Prewarm)
}

// maybeSpawnPrewarm is preparePostgres's deferred pool top-up: fires
// only after a successful exit that left templateName in place (cache
// hit, incremental/rollback/dump-only, or cold build — all set
// out.TemplateName; skip/branch-scoped outcomes don't).
func maybeSpawnPrewarm(
	cfg *config.Config,
	st *store.Store,
	repoID, worktreeID int64,
	d config.DatabaseConfig,
	fingerprint, templateName string,
	out Outcome,
	err error,
) {
	if err != nil || d.Prewarm == 0 || out.TemplateName != templateName {
		return
	}
	spawnPrewarm(cfg, st, repoID, worktreeID, fingerprint, templateName, d.Prewarm)
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
// Slot-name idempotence makes replenish self-healing: only missing
// `_spare<i>` slots are created, and slots beyond n (config shrank)
// are reaped best-effort.
func spawnPrewarm(
	cfg *config.Config,
	st *store.Store,
	repoID, worktreeID int64,
	fingerprint, templateName string,
	n uint32,
) {
	if n == 0 || cfg.Connections.Postgres == nil {
		return
	}
	if _, busy := prewarmInFlight.LoadOrStore(fingerprint, struct{}{}); busy {
		return
	}
	pg := *cfg.Connections.Postgres
	safego.Go("snapshot:prewarm", templateName, func() {
		defer prewarmInFlight.Delete(fingerprint)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		unpin := snapshot.Pin(fingerprint)
		defer unpin()

		drv, err := dbpostgres.Connect(ctx, pg)
		if err != nil {
			slog.Warn("prewarm connect", "template", templateName, "err", err)
			return
		}
		defer func() { _ = drv.Close() }()

		// The template can vanish between the triggering prepare and
		// this goroutine running (eviction raced the pin). Spares of a
		// dead template are unreachable, so just bail.
		if alive, _ := drv.DatabaseExists(ctx, templateName); !alive {
			return
		}

		created := 0
		for slot := 1; slot <= int(n); slot++ {
			name := snapshot.SpareName(templateName, slot)
			if exists, _ := drv.DatabaseExists(ctx, name); exists {
				continue
			}
			if err := drv.SnapshotRestore(ctx, templateName, name); err != nil {
				slog.Warn("prewarm restore", "spare", name, "template", templateName, "err", err)
				return
			}
			created++
		}

		// Reap slots beyond n so shrinking `prewarm` in config actually
		// shrinks the pool instead of leaving zombie spares around until
		// template eviction.
		if names, err := drv.ListMatching(ctx, templateName+snapshot.PrewarmSuffix); err == nil {
			for _, name := range names {
				if slot, ok := snapshot.SpareSlot(name, templateName); ok && slot > int(n) {
					if err := drv.DropDatabase(ctx, name); err != nil {
						slog.Warn("prewarm reap extra slot", "spare", name, "err", err)
					}
				}
			}
		}

		if created > 0 {
			_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsPrewarm,
				fmt.Sprintf("pre-warmed %d spare(s) for %s", created, templateName),
				repoID, worktreeID, "", 0, map[string]string{
					"engine":   "postgres",
					"template": templateName,
					"created":  strconv.Itoa(created),
					"pool":     strconv.Itoa(int(n)),
				})
		}
	})
}
