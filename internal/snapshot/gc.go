package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/engineconn"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/store"
)

// EvictExcess drops every cached template DB above
// `cfg.Snapshots.CapPerRepo` for the given repo, ordered
// by LRU (oldest `last_used_at` first). Called as a fire-and-forget
// goroutine after every `RecordSnapshot` so the inline cost is just
// a goroutine spawn — the engine-side DROP DATABASE runs in the
// background while the foreground completes the prepare.
//
// Errors are logged at WARN level and swallowed: a stale template
// row left behind is harmless (the next prepare with the same
// fingerprint will hit it; if the DB was already dropped the
// `DatabaseExists` check in prepare will fall back to a cold build
// and overwrite).
//
// Safe to invoke when the daemon-side periodic sweep is also
// running — the SQLite UPSERT on `RecordSnapshot` is idempotent and
// `DROP DATABASE IF EXISTS` is engine-side idempotent.
func EvictExcess(ctx context.Context, cfg *config.Config, st *store.Store, repoID int64) {
	capPerRepo := cfg.Snapshots.CapPerRepo
	candidates, err := st.ListLRUEvictable(ctx, repoID, capPerRepo)
	if err != nil {
		slog.Warn("snapshot eviction lookup", "repo_id", repoID, "err", err)
		return
	}
	evictCandidates(ctx, cfg, st, candidates, repoID, store.EvtSnapshotsEvictCap, "snapshot eviction",
		func(c store.SnapshotEvictionCandidate) string {
			return fmt.Sprintf("evicted %s (%s)", c.TemplateName, c.Engine)
		})
}

// evictCandidates drops each candidate's engine-side template and SQLite
// row, then writes one eviction event. Shared by the LRU / per-source /
// age sweeps — the only things that vary between them are the candidate
// query (done by the caller), the repo scope, the event type, the log
// prefix, and the message text.
//
// Pinned fingerprints (held by an in-flight prepare) are skipped — the
// sweep is best-effort and the next tick picks them up once the pin
// clears. A per-candidate drop/delete failure is logged and skipped so
// one bad row can't strand the rest; the row survives for a later retry.
//
// The size sweep (running-total accounting) and PurgeRepo (no pin check,
// error-collecting) keep their own loops.
func evictCandidates(
	ctx context.Context, cfg *config.Config, st *store.Store,
	cands []store.SnapshotEvictionCandidate, repoID int64,
	eventType, logPrefix string, msg func(store.SnapshotEvictionCandidate) string,
) {
	for _, c := range cands {
		if IsPinned(c.Fingerprint) {
			continue
		}
		if err := dropTemplate(ctx, cfg, c); err != nil {
			slog.Warn(logPrefix+" drop", "template", c.TemplateName, "engine", c.Engine, "err", err)
			continue
		}
		if err := st.DeleteSnapshot(ctx, c.Fingerprint); err != nil {
			slog.Warn(logPrefix+" delete row", "fp", c.Fingerprint, "template", c.TemplateName, "err", err)
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, eventType, msg(c),
			repoID, 0, "", 0, map[string]string{
				"engine":      c.Engine,
				"template":    c.TemplateName,
				"source_db":   c.SourceDB,
				"fingerprint": c.Fingerprint,
			})
	}
}

// PurgeRepo drops every cached template for the given repo and
// removes the corresponding snapshot rows. Used by MCP
// `snapshots_purge` (and any future CLI surface) when the user wants
// the next prepare to rebuild from scratch — e.g. after a schema
// migration framework changes its dump format.
//
// Returns the count of rows dropped + a multi-error of any per-row
// failures. Continues past per-row failures so a single bad engine
// row doesn't strand the rest.
func PurgeRepo(ctx context.Context, cfg *config.Config, st *store.Store, repoID int64) (dropped int, errs []error) {
	cands, err := st.ListSnapshotsForRepo(ctx, repoID)
	if err != nil {
		return 0, []error{err}
	}
	for _, c := range cands {
		if err := dropTemplate(ctx, cfg, c); err != nil {
			errs = append(errs, fmt.Errorf("drop %s (%s): %w", c.TemplateName, c.Engine, err))
			continue
		}
		if err := st.DeleteSnapshot(ctx, c.Fingerprint); err != nil {
			errs = append(errs, fmt.Errorf("delete row %s: %w", c.Fingerprint, err))
			continue
		}
		dropped++
		_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsPurge,
			fmt.Sprintf("purged %s (%s)", c.TemplateName, c.Engine),
			repoID, 0, "", 0, map[string]string{
				"engine":      c.Engine,
				"template":    c.TemplateName,
				"source_db":   c.SourceDB,
				"fingerprint": c.Fingerprint,
			})
	}
	return dropped, errs
}

// SweepBySource evicts cached templates that exceed
// `cfg.Snapshots.KeepPerSource` per source, where a "source" is the
// migration-content key (`migrations_hash`). Within each source the N
// most-recently-used templates are kept; older ones are dropped. Bounds
// the per-source template fan-out that accumulates as a project's
// dump/lockfile/engine-version churn while migration content holds
// steady. Runs as part of the daemon's periodic GC tick.
func SweepBySource(ctx context.Context, cfg *config.Config, st *store.Store) {
	keep := cfg.Snapshots.KeepPerSource
	if keep == 0 {
		return
	}
	cands, err := st.ListSnapshotsBeyondPerSource(ctx, keep)
	if err != nil {
		slog.Warn("snapshot source sweep query", "err", err)
		return
	}
	evictCandidates(ctx, cfg, st, cands, 0, store.EvtSnapshotsEvictSource, "snapshot source sweep",
		func(c store.SnapshotEvictionCandidate) string {
			return fmt.Sprintf("evicted %s (over keep_per_source=%d)", c.TemplateName, keep)
		})
}

func dropTemplate(ctx context.Context, cfg *config.Config, c store.SnapshotEvictionCandidate) error {
	fam, ok := engine.Canonical(c.Engine)
	if !ok {
		return fmt.Errorf("eviction: unsupported engine %q", c.Engine)
	}
	conn, configured, err := engineconn.Connect(ctx, cfg, fam)
	if !configured {
		return fmt.Errorf("connections.%s not configured", fam)
	}
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.DropSnapshot(ctx, c.TemplateName); err != nil {
		return err
	}
	// Reap the template's pre-warmed spare family too — spares are
	// anonymous engine-side copies with no SQLite row of their own, so
	// nothing else would ever collect them once the template is gone.
	// Prefix-reap is a no-op for engines/templates without spares.
	if _, err := conn.DropMatching(ctx, c.TemplateName+PrewarmSuffix); err != nil {
		return fmt.Errorf("drop spare family %s%s*: %w", c.TemplateName, PrewarmSuffix, err)
	}
	return nil
}

// SweepByAge drops every cached template whose `last_used_at` is
// older than `cfg.Snapshots.MaxAgeDays` days. Runs as
// part of the daemon's periodic GC tick. Cheap on small tables;
// keep an eye on it if the snapshots table grows past a few thousand
// rows.
func SweepByAge(ctx context.Context, cfg *config.Config, st *store.Store) {
	days := cfg.Snapshots.MaxAgeDays
	if days == 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	cands, err := st.ListSnapshotsOlderThan(ctx, cutoff)
	if err != nil {
		slog.Warn("snapshot age sweep query", "err", err)
		return
	}
	evictCandidates(ctx, cfg, st, cands, 0, store.EvtSnapshotsEvictAge, "snapshot age sweep",
		func(c store.SnapshotEvictionCandidate) string {
			return fmt.Sprintf("evicted %s (older than %dd)", c.TemplateName, days)
		})
}

// SweepBySize evicts the largest cached templates until total
// `size_bytes` falls below `cfg.Snapshots.MaxTotalGb`.
// Snapshots with size_bytes = NULL (never recorded) are evicted
// last — they're treated as size 0 by the ORDER BY in the store
// query.
func SweepBySize(ctx context.Context, cfg *config.Config, st *store.Store) {
	gb := cfg.Snapshots.MaxTotalGb
	if gb == 0 {
		return
	}
	capBytes := int64(gb) * 1024 * 1024 * 1024
	total, err := st.SumSnapshotBytes(ctx)
	if err != nil {
		slog.Warn("snapshot size sweep sum", "err", err)
		return
	}
	if total <= capBytes {
		return
	}
	cands, sizes, err := st.ListSnapshotsLargestLRU(ctx)
	if err != nil {
		slog.Warn("snapshot size sweep query", "err", err)
		return
	}
	for i, c := range cands {
		if total <= capBytes {
			break
		}
		if IsPinned(c.Fingerprint) {
			continue
		}
		if err := dropTemplate(ctx, cfg, c); err != nil {
			slog.Warn("snapshot size sweep drop", "template", c.TemplateName, "err", err)
			continue
		}
		if err := st.DeleteSnapshot(ctx, c.Fingerprint); err != nil {
			slog.Warn("snapshot size sweep delete row",
				"fp", c.Fingerprint, "template", c.TemplateName, "err", err)
		}
		total -= sizes[i]
		_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtSnapshotsEvictSize,
			fmt.Sprintf("evicted %s (size=%d)", c.TemplateName, sizes[i]),
			0, 0, "", 0, map[string]string{
				"engine":      c.Engine,
				"template":    c.TemplateName,
				"fingerprint": c.Fingerprint,
			})
	}
}
