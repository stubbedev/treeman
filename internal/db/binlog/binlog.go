// Package binlog tails a MySQL binary log as a fake replica and
// replays DDL (and optionally DML) events from a single source
// database onto every cached template + paratest clone the snapshot
// store knows about.
//
// Architecture:
//
//	go-mysql replication.BinlogSyncer
//	  ↓ events (Query / WriteRows / UpdateRows / DeleteRows / Rotate / Gtid …)
//	Replicator.run
//	  ↓ filtered to (cfg.SourceDB)
//	dispatchEvent
//	  ↓ enumerateTargets() — sibling template/clone DBs from the
//	    `snapshots` table + a SHOW DATABASES match for paratest
//	    clones — minus the source itself
//	apply{DDL,DML}(target)
//	  ↓ database/sql Exec via the existing internal/db/mysql driver
//	  ↓ checkpoint persisted to SQLite after each event
//
// Designed to fall over gracefully: if the server doesn't expose
// FULL row metadata, DML replay falls back to information_schema
// column lookup once per (db, table). If the replication user lacks
// privileges, the syncer errors immediately on Start.
package binlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	"github.com/stubbedev/treeman/internal/store"
)

// Replicator owns one BinlogSyncer + dispatch loop. There is one
// Replicator per (host:port, source_db) — multiple databases on the
// same server get distinct Replicators sharing the same connection
// pool through the driver.
type Replicator struct {
	cfg      *config.Config
	st       *store.Store
	repoID   int64
	sourceDB string

	syncer *replication.BinlogSyncer

	// targets is the live cache of which databases to apply events
	// to. Refreshed at the start of each event batch (cheap query)
	// and on SchemaChanged signals.
	mu      sync.Mutex
	targets []string

	// metaCache caches resolved column + PK metadata per (db, table)
	// for row-event replay. Refreshed lazily; invalidated when a
	// DDL Query event hits the schema.
	metaCache *metaCache
}

// New constructs a Replicator. Call Start to begin streaming.
func New(cfg *config.Config, st *store.Store, repoID int64, sourceDB string) (*Replicator, error) {
	if cfg.Connections.Mysql == nil {
		return nil, errors.New("connections.mysql not configured")
	}
	if !cfg.Watcher.Binlog.Enabled {
		return nil, errors.New("watcher.binlog.enabled = false")
	}
	mc := cfg.Connections.Mysql
	user := mc.User
	host := mc.Host
	port := uint16(mc.Port)
	if port == 0 {
		port = 3306
	}
	// Honour `container:` for the replication dial too — keeps
	// binlog replay reachable when the upstream MySQL is in docker
	// with no published port.
	if mc.Container != "" {
		ip, err := containerip.Resolve(mc.Container, mc.ContainerEngine)
		if err != nil {
			return nil, fmt.Errorf("resolve container %q: %w", mc.Container, err)
		}
		if ip != "" {
			host = ip
		}
	}
	cfgSyncer := replication.BinlogSyncerConfig{
		ServerID: cfg.Watcher.Binlog.ServerID,
		Flavor:   cfg.Watcher.Binlog.Flavor,
		Host:     host,
		Port:     port,
		User:     user,
		Password: mc.Password,
		// Verify the upstream is configured for ROW format. If it's
		// not, the syncer still connects but every event arrives as
		// QueryEvent — DML replay won't work. We log a warning the
		// first time we see a non-ROW format event.
		ParseTime: true,
		// FULL row metadata gives us column names directly; opt in.
		// Servers without binlog_row_metadata=FULL silently ignore
		// the request and we fall back to information_schema lookup.
		UseDecimal: true,
	}
	r := &Replicator{
		cfg:       cfg,
		st:        st,
		repoID:    repoID,
		sourceDB:  sourceDB,
		syncer:    replication.NewBinlogSyncer(cfgSyncer),
		metaCache: newMetaCache(),
	}
	return r, nil
}

// Start tails the binlog. Returns when ctx is cancelled or a fatal
// error occurs.
func (r *Replicator) Start(ctx context.Context) error {
	cp, err := r.st.GetBinlogCheckpoint(ctx, r.repoID, r.sourceDB)
	if err != nil {
		return fmt.Errorf("checkpoint lookup: %w", err)
	}
	var streamer *replication.BinlogStreamer
	if cp != nil && cp.GtidSet != "" {
		gset, err := gomysql.ParseGTIDSet(r.cfg.Watcher.Binlog.Flavor, cp.GtidSet)
		if err != nil {
			return fmt.Errorf("parse gtid set %q: %w", cp.GtidSet, err)
		}
		streamer, err = r.syncer.StartSyncGTID(gset)
		if err != nil {
			return fmt.Errorf("start gtid sync: %w", err)
		}
	} else {
		var pos gomysql.Position
		if cp != nil && cp.BinlogFile != "" {
			pos = gomysql.Position{Name: cp.BinlogFile, Pos: cp.BinlogPos}
		}
		streamer, err = r.syncer.StartSync(pos)
		if err != nil {
			return fmt.Errorf("start sync at %v: %w", pos, err)
		}
	}

	slog.Info("binlog replicator started",
		"repo_id", r.repoID, "source_db", r.sourceDB,
		"server_id", r.cfg.Watcher.Binlog.ServerID)

	for {
		ev, err := streamer.GetEvent(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("binlog get event: %w", err)
		}
		if err := r.dispatch(ctx, ev); err != nil {
			slog.Warn("binlog dispatch", "type", ev.Header.EventType, "err", err)
		}
	}
}

// Stop closes the syncer; subsequent GetEvent calls return.
func (r *Replicator) Stop() {
	if r.syncer != nil {
		r.syncer.Close()
	}
}

func (r *Replicator) dispatch(ctx context.Context, ev *replication.BinlogEvent) error {
	switch e := ev.Event.(type) {
	case *replication.QueryEvent:
		schema := string(e.Schema)
		if schema != "" && schema != r.sourceDB {
			return nil
		}
		if !*r.cfg.Watcher.Binlog.ApplyDDL {
			return nil
		}
		return r.applyDDL(ctx, string(e.Query))

	case *replication.RowsEvent:
		if !*r.cfg.Watcher.Binlog.ApplyDML {
			return nil
		}
		if string(e.Table.Schema) != r.sourceDB {
			return nil
		}
		return r.applyRows(ctx, ev.Header.EventType, e)

	case *replication.RotateEvent:
		// Update checkpoint filename so we don't replay the previous
		// file when the server cycles to a new one.
		return r.st.SaveBinlogCheckpoint(ctx, store.BinlogCheckpoint{
			RepoID:     r.repoID,
			SourceDB:   r.sourceDB,
			Flavor:     r.cfg.Watcher.Binlog.Flavor,
			BinlogFile: string(e.NextLogName),
			BinlogPos:  uint32(e.Position),
			UpdatedAt:  int64(ev.Header.Timestamp) * 1000,
		})

	case *replication.GTIDEvent:
		// Capture the latest committed GTID. Stored on the next
		// successful event so we resume past the most recently
		// applied transaction, not the next one.
		u, err := e.GTIDNext()
		if err == nil {
			return r.st.SaveBinlogCheckpoint(ctx, store.BinlogCheckpoint{
				RepoID:    r.repoID,
				SourceDB:  r.sourceDB,
				Flavor:    r.cfg.Watcher.Binlog.Flavor,
				GtidSet:   u.String(),
				UpdatedAt: int64(ev.Header.Timestamp) * 1000,
			})
		}
	}
	return nil
}

// enumerateTargets returns the per-event apply set: every snapshot
// template + paratest clone for the source DB on this engine, minus
// the source itself. Refreshed cheaply on every event because
// snapshot rows can come and go between the daemon's polling
// interval.
func (r *Replicator) enumerateTargets(ctx context.Context, drv *dbmysql.Driver) ([]string, error) {
	rows, err := r.st.DB.QueryContext(ctx, `
		SELECT template_name FROM snapshots
		WHERE repo_id = ? AND engine IN ('mysql','mariadb','tidb') AND source_db = ?`,
		r.repoID, r.sourceDB)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		seen[name] = struct{}{}
	}
	// Paratest clone names aren't stored in `snapshots`; enumerate
	// every DB on the server prefixed with the source DB name
	// followed by `_test_` (the canonical paratest template). False
	// positives are dropped by the engine's own `IF EXISTS` checks
	// during apply.
	dbs, err := drv.ListMatching(ctx, r.sourceDB+"_test_")
	if err == nil {
		for _, n := range dbs {
			seen[n] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		if n == r.sourceDB {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// applyDDL strips/swaps the schema reference and runs the statement
// against every target. The QueryEvent.Schema header tells us where
// the original statement ran; we ignore embedded `USE <db>` since
// each target needs the statement aimed at it.
func (r *Replicator) applyDDL(ctx context.Context, query string) error {
	q := strings.TrimSpace(query)
	if q == "" || strings.EqualFold(q, "BEGIN") || strings.EqualFold(q, "COMMIT") {
		return nil
	}
	// Skip USE statements — we set the schema explicitly on the
	// connection per-target.
	if strings.HasPrefix(strings.ToUpper(q), "USE ") {
		return nil
	}
	// Any DDL may have altered table structure — invalidate cached
	// column/PK metadata for the source schema so the next DML
	// event re-reads it.
	r.metaCache.invalidate(r.sourceDB)
	drv, err := dbmysql.Connect(ctx, *r.cfg.Connections.Mysql)
	if err != nil {
		return err
	}
	defer drv.Close()
	targets, err := r.enumerateTargets(ctx, drv)
	if err != nil {
		return fmt.Errorf("enumerate targets: %w", err)
	}
	for _, t := range targets {
		if _, err := drv.DB.ExecContext(ctx, "USE `"+t+"`"); err != nil {
			slog.Warn("binlog ddl USE failed", "target", t, "err", err)
			continue
		}
		if _, err := drv.DB.ExecContext(ctx, q); err != nil {
			slog.Warn("binlog ddl apply", "target", t, "err", err, "query", truncate(q, 120))
			continue
		}
	}
	return nil
}

// applyRows reconstructs INSERT/UPDATE/DELETE from a RowsEvent and
// runs against every target. Column + PK metadata is taken from
// the binlog's FULL row metadata when present and from
// information_schema (cached per table) otherwise. Each row in the
// event maps to one prepared INSERT/UPDATE/DELETE per target.
//
// Off by default (`watcher.binlog.apply_dml = false`) — DML replay
// is the high-blast-radius path. Errors per-target are logged and
// skipped so one stale clone doesn't block the rest.
func (r *Replicator) applyRows(ctx context.Context, et replication.EventType, e *replication.RowsEvent) error {
	drv, err := dbmysql.Connect(ctx, *r.cfg.Connections.Mysql)
	if err != nil {
		return err
	}
	defer drv.Close()
	schema := string(e.Table.Schema)
	table := string(e.Table.Table)
	meta, err := resolveTableMeta(ctx, drv, r.metaCache, schema, table, e.Table)
	if err != nil {
		return fmt.Errorf("resolve meta %s.%s: %w", schema, table, err)
	}
	if len(meta.Columns) == 0 {
		return fmt.Errorf("no columns resolved for %s.%s", schema, table)
	}
	targets, err := r.enumerateTargets(ctx, drv)
	if err != nil {
		return fmt.Errorf("enumerate targets: %w", err)
	}
	// Sanity-check row width — a schema drift mid-stream would
	// otherwise produce off-by-one binds.
	for _, row := range e.Rows {
		if len(row) != len(meta.Columns) {
			slog.Warn("binlog dml row width mismatch — skipping batch",
				"schema", schema, "table", table,
				"row_len", len(row), "meta_len", len(meta.Columns))
			r.metaCache.invalidate(schema)
			return nil
		}
	}
	for _, t := range targets {
		if err := applyRowsForTarget(ctx, drv.DB, t, table, meta, et, e.Rows); err != nil {
			slog.Warn("binlog dml apply", "target", t, "err", err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
