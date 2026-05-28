package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
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
	cap := cfg.Snapshots.CapPerRepo
	candidates, err := st.ListLRUEvictable(ctx, repoID, cap)
	if err != nil {
		slog.Warn("snapshot eviction lookup", "repo_id", repoID, "err", err)
		return
	}
	if len(candidates) == 0 {
		return
	}
	for _, c := range candidates {
		if err := dropTemplate(ctx, cfg, c); err != nil {
			slog.Warn("snapshot eviction drop", "template", c.TemplateName,
				"engine", c.Engine, "err", err)
			// Continue so a missing row for one engine doesn't block
			// pruning others. The row stays so a retry can pick it
			// up next time.
			continue
		}
		if err := st.DeleteSnapshot(ctx, c.Fingerprint); err != nil {
			slog.Warn("snapshot eviction delete row", "fp", c.Fingerprint, "err", err)
		}
		_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_evict",
			fmt.Sprintf("evicted %s (%s)", c.TemplateName, c.Engine),
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
		_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_purge",
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

func dropTemplate(ctx context.Context, cfg *config.Config, c store.SnapshotEvictionCandidate) error {
	switch c.Engine {
	case "mysql", "mariadb", "tidb":
		if cfg.Connections.Mysql == nil {
			return fmt.Errorf("connections.mysql not configured")
		}
		drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
		if err != nil {
			return err
		}
		defer drv.Close()
		return drv.DropSnapshot(ctx, c.TemplateName)
	case "postgres", "postgresql":
		if cfg.Connections.Postgres == nil {
			return fmt.Errorf("connections.postgres not configured")
		}
		drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
		if err != nil {
			return err
		}
		defer drv.Close()
		return drv.DropSnapshot(ctx, c.TemplateName)
	case "mongodb":
		if cfg.Connections.Mongodb == nil {
			return fmt.Errorf("connections.mongodb not configured")
		}
		drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
		if err != nil {
			return err
		}
		defer drv.Close(ctx)
		return drv.DropSnapshot(ctx, c.TemplateName)
	case "elasticsearch":
		if cfg.Connections.Elasticsearch == nil {
			return fmt.Errorf("connections.elasticsearch not configured")
		}
		drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if err != nil {
			return err
		}
		return drv.DropSnapshot(ctx, c.TemplateName)
	case "redis":
		if cfg.Connections.Redis == nil {
			return fmt.Errorf("connections.redis not configured")
		}
		drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
		if err != nil {
			return err
		}
		defer drv.Close()
		return drv.DropSnapshot(ctx, c.TemplateName)
	default:
		return fmt.Errorf("eviction: unsupported engine %q", c.Engine)
	}
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
	for _, c := range cands {
		if err := dropTemplate(ctx, cfg, c); err != nil {
			slog.Warn("snapshot age sweep drop", "template", c.TemplateName, "err", err)
			continue
		}
		_ = st.DeleteSnapshot(ctx, c.Fingerprint)
		_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_age_evict",
			fmt.Sprintf("evicted %s (older than %dd)", c.TemplateName, days),
			0, 0, "", 0, map[string]string{
				"engine":      c.Engine,
				"template":    c.TemplateName,
				"fingerprint": c.Fingerprint,
			})
	}
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
	cap := int64(gb) * 1024 * 1024 * 1024
	total, err := st.SumSnapshotBytes(ctx)
	if err != nil {
		slog.Warn("snapshot size sweep sum", "err", err)
		return
	}
	if total <= cap {
		return
	}
	cands, sizes, err := st.ListSnapshotsLargestLRU(ctx)
	if err != nil {
		slog.Warn("snapshot size sweep query", "err", err)
		return
	}
	for i, c := range cands {
		if total <= cap {
			break
		}
		if err := dropTemplate(ctx, cfg, c); err != nil {
			slog.Warn("snapshot size sweep drop", "template", c.TemplateName, "err", err)
			continue
		}
		_ = st.DeleteSnapshot(ctx, c.Fingerprint)
		total -= sizes[i]
		_ = st.WriteEvent(ctx, store.LevelInfo, "snapshot_size_evict",
			fmt.Sprintf("evicted %s (size=%d)", c.TemplateName, sizes[i]),
			0, 0, "", 0, map[string]string{
				"engine":      c.Engine,
				"template":    c.TemplateName,
				"fingerprint": c.Fingerprint,
			})
	}
}
