package snapshot

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/stubbedev/treeman/internal/config"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	"github.com/stubbedev/treeman/internal/store"
)

// EvictExcess drops every cached template DB above
// `cfg.Snapshots.Retention.CapPerRepo` for the given repo, ordered
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
	cap := cfg.Snapshots.Retention.CapPerRepo
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
	default:
		// Mongo/redis/es don't keep template snapshots on the cache
		// hot path yet — when they land, their engine-specific drop
		// calls go here.
		return fmt.Errorf("eviction: unsupported engine %q", c.Engine)
	}
}
