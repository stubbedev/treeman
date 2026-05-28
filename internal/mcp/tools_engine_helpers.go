package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/klauspost/compress/gzip"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/store"
)

// Alias so tools_engine.go can refer to the snapshot type without
// pulling the store import into its own header — keeps the engine
// tools file focused on MCP wiring.
type store_SnapshotRecord = store.SnapshotRecord

func snapshotLookupByFingerprint(ctx context.Context, st *store.Store, fp string) (*store.SnapshotRecord, error) {
	return st.LookupSnapshot(ctx, fp)
}

// snapshotLookupByEngineSource returns the most-recently-used
// snapshot matching (engine, source_db). Slow path compared to
// fingerprint lookup, but useful when the caller only knows the
// engine+source pair (e.g. the agent saw the names in YAML).
func snapshotLookupByEngineSource(ctx context.Context, st *store.Store, engine, sourceDB string) (*store.SnapshotRecord, error) {
	var fp string
	err := st.DB.QueryRowContext(ctx, `
		SELECT fingerprint FROM snapshots
		WHERE engine = ? AND source_db = ?
		ORDER BY last_used_at DESC LIMIT 1
	`, engine, sourceDB).Scan(&fp)
	if err != nil {
		return nil, nil
	}
	return st.LookupSnapshot(ctx, fp)
}

func snapshotRecordToMap(r *store.SnapshotRecord) map[string]any {
	return map[string]any{
		"fingerprint":     r.Fingerprint,
		"engine":          r.Engine,
		"engine_version":  r.EngineVersion,
		"source_db":       r.SourceDB,
		"template_name":   r.TemplateName,
		"migrations_hash": r.MigrationsHash,
		"dump_hash":       r.DumpHash,
		"lockfile_hashes": r.LockfileHashes,
		"repo_id":         r.RepoID,
		"size_bytes":      r.SizeBytes,
		"created_at":      r.CreatedAt,
		"last_used_at":    r.LastUsedAt,
		"use_count":       r.UseCount,
	}
}

// probeTemplate checks whether the engine-side template exists and
// returns a size estimate where the engine exposes one. Errors are
// silently dropped (exists=false) — the caller surfaces them via the
// SQLite row's recorded state.
func probeTemplate(ctx context.Context, cfg *config.Config, engine, template string) (bool, int64, string) {
	switch engine {
	case "mysql", "mariadb", "tidb":
		if cfg.Connections.Mysql == nil {
			return false, 0, ""
		}
		drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
		if err != nil {
			return false, 0, ""
		}
		defer func() { _ = drv.Close() }()
		exists, _ := drv.DatabaseExists(ctx, template)
		ver, _ := drv.EngineVersion(ctx)
		if !exists {
			return false, 0, ver
		}
		// SUM(data_length+index_length) — bytes-on-disk per
		// information_schema.tables.
		var size int64
		_ = drv.DB.QueryRowContext(ctx, `
			SELECT IFNULL(SUM(data_length + index_length), 0)
			FROM information_schema.tables WHERE table_schema = ?
		`, template).Scan(&size)
		return true, size / 1024, ver
	case "postgres", "postgresql":
		if cfg.Connections.Postgres == nil {
			return false, 0, ""
		}
		drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
		if err != nil {
			return false, 0, ""
		}
		defer func() { _ = drv.Close() }()
		exists, _ := drv.DatabaseExists(ctx, template)
		ver, _ := drv.EngineVersion(ctx)
		if !exists {
			return false, 0, ver
		}
		var size int64
		_ = drv.DB.QueryRowContext(ctx, "SELECT pg_database_size($1)/1024", template).Scan(&size)
		return true, size, ver
	case "mongodb":
		if cfg.Connections.Mongodb == nil {
			return false, 0, ""
		}
		drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
		if err != nil {
			return false, 0, ""
		}
		defer func() { _ = drv.Close(ctx) }()
		exists, _ := drv.DatabaseExists(ctx, template)
		ver, _ := drv.EngineVersion(ctx)
		return exists, 0, ver
	case "redis":
		if cfg.Connections.Redis == nil {
			return false, 0, ""
		}
		drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
		if err != nil {
			return false, 0, ""
		}
		defer func() { _ = drv.Close() }()
		ok, _ := drv.PrefixExists(ctx, template)
		ver, _ := drv.EngineVersion(ctx)
		return ok, 0, ver
	case "elasticsearch", "opensearch":
		if cfg.Connections.Elasticsearch == nil {
			return false, 0, ""
		}
		drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if err != nil {
			return false, 0, ""
		}
		matches, _ := drv.ListMatching(ctx, template)
		ver, _ := drv.EngineVersion(ctx)
		return len(matches) > 0, 0, ver
	}
	return false, 0, ""
}

// reservedTemplatePrefixes are the namespace markers treeman owns:
// cache templates (`_tm_<hex>` / `tm_<hex>` for ES) and branch-scoped
// durable copies (`_tmbs_<hex>` / `tmbs_<hex>` for ES). MCP
// snapshot_drop must refuse anything else so a hand-crafted or
// typo'd template arg can't reach DropMatching's prefix LIKE and
// reap unrelated app databases.
var reservedTemplatePrefixes = []string{"_tm_", "_tmbs_", "tm_", "tmbs_"}

func isReservedTemplateName(name string) bool {
	for _, p := range reservedTemplatePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// dropTemplate removes the engine-side template for one fingerprint.
// Mirrors the per-engine teardown in internal/prepare but keyed by
// the cached template name rather than a worktree slug.
//
// Refuses to act on names that don't carry a treeman-reserved prefix
// (`_tm_`, `_tmbs_`, `tm_`, `tmbs_`). DropMatching/DropPrefix do a
// prefix LIKE/scan; an MCP caller passing a non-reserved string
// would otherwise reap whatever app namespaces happen to share that
// prefix.
func dropTemplate(ctx context.Context, cfg *config.Config, eng, template string) error {
	if !isReservedTemplateName(template) {
		return fmt.Errorf("refusing to drop %q: not a treeman-reserved template name (expected one of %v)", template, reservedTemplatePrefixes)
	}
	fam, ok := engine.Canonical(eng)
	if !ok {
		return fmt.Errorf("unknown engine %q (allowed: %s)", eng, engine.KnownList())
	}
	switch fam {
	case engine.FamilyMySQL:
		if cfg.Connections.Mysql == nil {
			return fmt.Errorf("connections.mysql not configured")
		}
		drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
		if err != nil {
			return err
		}
		defer func() { _ = drv.Close() }()
		_, err = drv.DropMatching(ctx, template)
		return err
	case engine.FamilyPostgres:
		if cfg.Connections.Postgres == nil {
			return fmt.Errorf("connections.postgres not configured")
		}
		drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
		if err != nil {
			return err
		}
		defer func() { _ = drv.Close() }()
		_, err = drv.DropMatching(ctx, template)
		return err
	case engine.FamilyMongo:
		if cfg.Connections.Mongodb == nil {
			return fmt.Errorf("connections.mongodb not configured")
		}
		drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
		if err != nil {
			return err
		}
		defer func() { _ = drv.Close(ctx) }()
		_, err = drv.DropMatching(ctx, template)
		return err
	case engine.FamilyRedis:
		if cfg.Connections.Redis == nil {
			return fmt.Errorf("connections.redis not configured")
		}
		drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
		if err != nil {
			return err
		}
		defer func() { _ = drv.Close() }()
		_, err = drv.DropPrefix(ctx, template)
		return err
	case engine.FamilyES:
		if cfg.Connections.Elasticsearch == nil {
			return fmt.Errorf("connections.elasticsearch not configured")
		}
		drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
		if err != nil {
			return err
		}
		_, err = drv.DropMatching(ctx, template)
		return err
	default:
		return fmt.Errorf("unsupported engine family %q", fam)
	}
}

// ─── dump generators ────────────────────────────────────────────────

func runMysqldump(ctx context.Context, conn *config.MysqlConn, db, outPath string, gzipOut bool) (*mcpsdk.CallToolResult, dbDumpOut, error) {
	if _, err := exec.LookPath("mysqldump"); err != nil {
		return nil, dbDumpOut{}, fmt.Errorf("mysqldump not on PATH: %w", err)
	}
	args := []string{
		"--host=" + conn.Host,
		"--port=" + fmt.Sprintf("%d", coalescePort(conn.Port, 3306)),
		"--user=" + conn.User,
		"--single-transaction", "--routines", "--triggers",
		db,
	}
	cmd := exec.CommandContext(ctx, "mysqldump", args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+conn.Password)
	return runDumpCmd(cmd, outPath, gzipOut)
}

func runPgDump(ctx context.Context, conn *config.PostgresConn, db, outPath string, gzipOut bool) (*mcpsdk.CallToolResult, dbDumpOut, error) {
	if _, err := exec.LookPath("pg_dump"); err != nil {
		return nil, dbDumpOut{}, fmt.Errorf("pg_dump not on PATH: %w", err)
	}
	args := []string{
		"--host=" + conn.Host,
		"--port=" + fmt.Sprintf("%d", coalescePort(conn.Port, 5432)),
		"--username=" + conn.User,
		"--no-password", "--format=plain", "--clean", "--if-exists", db,
	}
	cmd := exec.CommandContext(ctx, "pg_dump", args...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+conn.Password)
	return runDumpCmd(cmd, outPath, gzipOut)
}

// runESDump writes a `_bulk`-format NDJSON dump of every doc under
// `<prefix>*` indices. Round-trips through Driver.Restore (which
// substitutes `{target_db}` back to the destination worktree's
// prefix at load time).
func runESDump(ctx context.Context, conn *config.EsConn, prefix, outPath string, gzipOut bool) (*mcpsdk.CallToolResult, dbDumpOut, error) {
	if conn == nil {
		return nil, dbDumpOut{}, fmt.Errorf("connections.elasticsearch not configured")
	}
	drv, err := dbes.Connect(ctx, *conn)
	if err != nil {
		return nil, dbDumpOut{}, err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return nil, dbDumpOut{}, err
	}
	var w io.WriteCloser = f
	if gzipOut {
		w = gzip.NewWriter(f)
	}
	if err := drv.Dump(ctx, prefix, w); err != nil {
		if gzipOut {
			_ = w.Close()
		}
		_ = f.Close()
		return nil, dbDumpOut{}, fmt.Errorf("es dump: %w", err)
	}
	// Flush+close on the success path with the error checked: a
	// failed gzip footer or final file write would otherwise leave a
	// truncated dump that we'd report as a success.
	if gzipOut {
		if err := w.Close(); err != nil {
			return nil, dbDumpOut{}, fmt.Errorf("finalize gzip dump %s: %w", outPath, err)
		}
	}
	if err := f.Close(); err != nil {
		return nil, dbDumpOut{}, fmt.Errorf("finalize dump %s: %w", outPath, err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return nil, dbDumpOut{}, err
	}
	return nil, dbDumpOut{Path: outPath, SizeBytes: info.Size()}, nil
}

func runMongoDump(ctx context.Context, conn *config.MongoConn, db, outPath string, gzipOut bool) (*mcpsdk.CallToolResult, dbDumpOut, error) {
	if _, err := exec.LookPath("mongodump"); err != nil {
		return nil, dbDumpOut{}, fmt.Errorf("mongodump not on PATH: %w", err)
	}
	args := []string{
		"--uri=" + conn.URI,
		"--db=" + db,
		"--archive=" + outPath,
		"--quiet",
	}
	if gzipOut {
		args = append(args, "--gzip")
	}
	cmd := exec.CommandContext(ctx, "mongodump", args...)
	if err := cmd.Run(); err != nil {
		return nil, dbDumpOut{}, fmt.Errorf("mongodump: %w", err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return nil, dbDumpOut{}, err
	}
	return nil, dbDumpOut{Path: outPath, SizeBytes: info.Size()}, nil
}

// runDumpCmd streams cmd.Stdout to outPath, optionally gzip-compressed
// in-process. Returns the final byte count.
func runDumpCmd(cmd *exec.Cmd, outPath string, gzipOut bool) (*mcpsdk.CallToolResult, dbDumpOut, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return nil, dbDumpOut{}, err
	}
	var w io.WriteCloser = f
	if gzipOut {
		w = gzip.NewWriter(f)
	}
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if gzipOut {
			_ = w.Close()
		}
		_ = f.Close()
		return nil, dbDumpOut{}, fmt.Errorf("%s: %w: %s", cmd.Path, err, stderr.String())
	}
	// Flush+close on the success path with the error checked: a
	// failed gzip footer or final file write would otherwise leave a
	// truncated dump that we'd report as a success.
	if gzipOut {
		if err := w.Close(); err != nil {
			return nil, dbDumpOut{}, fmt.Errorf("finalize gzip dump %s: %w", outPath, err)
		}
	}
	if err := f.Close(); err != nil {
		return nil, dbDumpOut{}, fmt.Errorf("finalize dump %s: %w", outPath, err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return nil, dbDumpOut{}, err
	}
	return nil, dbDumpOut{Path: outPath, SizeBytes: info.Size()}, nil
}

func coalescePort(v uint16, dflt int) int {
	if v == 0 {
		return dflt
	}
	return int(v)
}
