package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	"github.com/stubbedev/treeman/internal/db/ident"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/resolve"
)

// registerEngineTools binds the engine-introspection + snapshot
// inspection tools. Called from registerReadTools (read-only ones)
// and registerWriteTools (mutating ones).
func registerEngineReadTools(srv *mcpsdk.Server) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "engine_status",
		Description: "Probe every engine declared in .treeman.yaml. For each: reachable? version? per-DB summary (database list for mysql/postgres/mongo; index list for ES; DBSIZE for redis). Call this when the user asks \"are my databases up?\" or before driving prepare_run/worktree_create against a fresh environment.",
		Annotations: readOnlyAnno("Engine status probe", true),
	}, engineStatusTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "db_schema_dump",
		Description: "Return the live schema for ONE database on a configured engine. mysql/postgres → every table's CREATE TABLE. mongodb → collection list + first-doc samples. elasticsearch → index mapping JSON. redis → SCAN-driven key-pattern summary. engine = the engine string from databases[].engine; db = the rendered per-worktree database/prefix name (use snapshot_inspect or worktree_show to find it). Use when reasoning about live shape vs. what migrations expect.",
		Annotations: readOnlyAnno("Dump live schema", true),
	}, dbSchemaDumpTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "db_query",
		Description: "Run a READ-ONLY query against a configured engine. SQL engines (mysql/postgres) — only SELECT/SHOW/EXPLAIN/DESCRIBE/WITH accepted; mutations are REFUSED with an error. MongoDB → find-style filter JSON against a named collection. Elasticsearch → JSON _search body against an index. Redis → one command from {GET, MGET, SMEMBERS, HGETALL, KEYS, SCAN, EXISTS, TYPE, TTL, LRANGE, ZRANGE, HKEYS, HVALS, HGET, HMGET, DBSIZE, INFO, PING}. Returns rows/docs/hits as JSON. Use for inspecting live data or verifying a migration's effect.",
		Annotations: readOnlyAnno("Run read-only query", true),
	}, dbQueryTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "snapshot_inspect",
		Description: "Resolve ONE snapshot (by fingerprint, or by engine+source_db) and report: SQLite row contents, whether the engine-side template still exists, template size, engine version at snapshot time. Call this BEFORE snapshot_drop to confirm you're dropping the right one — and routinely when diagnosing \"cache hit but prepare still failed\": template_exists=false on a fingerprint means the row is an orphan and the next prepare will (correctly) cold-build.",
		Annotations: readOnlyAnno("Inspect snapshot", true),
	}, snapshotInspectTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "hook_log_read",
		Description: "Read the FULL hook log file for one (worktree, phase, group_idx). Hook logs live at <worktree>/.treeman-hooks/<phase>-<group>.log. logs_hooks / hook_runs only stores 16KB tails — use this when you need more context than the tail. max_bytes=N returns just the trailing N bytes (and flags truncated=true).",
		Annotations: readOnlyAnno("Read hook log", false),
	}, hookLogReadTool)
}

func registerEngineWriteTools(srv *mcpsdk.Server) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "snapshot_drop",
		Description: "Delete ONE snapshot by fingerprint. Drops the engine-side template (DB / index-prefix / key-prefix / collection-set) AND removes the SQLite row. The next prepare for that fingerprint will cold-rebuild. Use for evicting one stale entry without nuking the rest of the cache; the cache-cleanup prompt drives this for known-orphan sweeps. Call snapshot_inspect first to confirm you're dropping the right fingerprint.",
		Annotations: writeAnno("Drop snapshot", true, true, true),
	}, snapshotDropTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "db_dump",
		Description: "Generate a dump of a live engine database to disk. Supported engines: mysql (and mariadb/tidb aliases) -> mysqldump; postgres (and postgresql alias) -> pg_dump --format=plain --clean --if-exists; mongodb -> mongodump --archive; elasticsearch (and opensearch alias) -> scroll API NDJSON _bulk with {target_db} prefix substitution. Redis dumps are intentionally not implemented (redis cold-build uses a seed step, not a dump file, so there is no restore counterpart). output_dir defaults to <repo>/storage/dumps. Use this to refresh the seed dump treeman uses for cold builds (commit the new file then trigger prepare_run). Returns the absolute path + byte count.",
		Annotations: writeAnno("Dump database", false, false, true),
	}, dbDumpTool)
}

// ─── engine_status ──────────────────────────────────────────────────

type engineStatusIn struct {
	Repo string `json:"repo,omitempty"`
}
type engineStatusOut struct {
	Engines []engineProbeResult `json:"engines"`
}
type engineProbeResult struct {
	Engine     string         `json:"engine"`
	Configured bool           `json:"configured"`
	Reachable  bool           `json:"reachable"`
	Version    string         `json:"version,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	Error      string         `json:"error,omitempty"`
}

func engineStatusTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in engineStatusIn) (*mcpsdk.CallToolResult, engineStatusOut, error) {
	cfg, err := loadCfgForRepo(in.Repo)
	if err != nil {
		return nil, engineStatusOut{}, err
	}
	out := engineStatusOut{}
	out.Engines = append(out.Engines, probeMySQL(ctx, cfg))
	out.Engines = append(out.Engines, probePostgres(ctx, cfg))
	out.Engines = append(out.Engines, probeMongo(ctx, cfg))
	out.Engines = append(out.Engines, probeRedis(ctx, cfg))
	out.Engines = append(out.Engines, probeES(ctx, cfg))
	return nil, out, nil
}

func probeMySQL(ctx context.Context, cfg *config.Config) engineProbeResult {
	r := engineProbeResult{Engine: "mysql"}
	if cfg.Connections.Mysql == nil {
		return r
	}
	r.Configured = true
	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer func() { _ = drv.Close() }()
	r.Reachable = true
	r.Version, _ = drv.EngineVersion(ctx)
	dbs, _ := listMysqlDatabases(ctx, drv)
	r.Detail = databasesDetail(dbs)
	return r
}

func probePostgres(ctx context.Context, cfg *config.Config) engineProbeResult {
	r := engineProbeResult{Engine: "postgres"}
	if cfg.Connections.Postgres == nil {
		return r
	}
	r.Configured = true
	drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer func() { _ = drv.Close() }()
	r.Reachable = true
	r.Version, _ = drv.EngineVersion(ctx)
	dbs, _ := listPostgresDatabases(ctx, drv)
	r.Detail = databasesDetail(dbs)
	return r
}

func probeMongo(ctx context.Context, cfg *config.Config) engineProbeResult {
	r := engineProbeResult{Engine: "mongodb"}
	if cfg.Connections.Mongodb == nil {
		return r
	}
	r.Configured = true
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer func() { _ = drv.Close(ctx) }()
	r.Reachable = true
	r.Version, _ = drv.EngineVersion(ctx)
	dbs, err := drv.Client.ListDatabaseNames(ctx, bson.D{})
	if err == nil {
		r.Detail = databasesDetail(dbs)
	}
	return r
}

func probeRedis(ctx context.Context, cfg *config.Config) engineProbeResult {
	r := engineProbeResult{Engine: "redis"}
	if cfg.Connections.Redis == nil {
		return r
	}
	r.Configured = true
	drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer func() { _ = drv.Close() }()
	r.Reachable = true
	r.Version, _ = drv.EngineVersion(ctx)
	c := drv.Client()
	dbsize, err := c.DBSize(ctx).Result()
	if err == nil {
		r.Detail = map[string]any{"db0_keys": dbsize}
	}
	return r
}

func probeES(ctx context.Context, cfg *config.Config) engineProbeResult {
	r := engineProbeResult{Engine: "elasticsearch"}
	if cfg.Connections.Elasticsearch == nil {
		return r
	}
	r.Configured = true
	drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Reachable = true
	r.Version, _ = drv.EngineVersion(ctx)
	indices, _ := drv.ListMatching(ctx, "")
	var app, internal []string
	for _, idx := range indices {
		if isESInternalIndex(idx) {
			internal = append(internal, idx)
		} else {
			app = append(app, idx)
		}
	}
	r.Detail = map[string]any{"indices": app}
	if len(internal) > 0 {
		r.Detail["internal_count"] = len(internal)
		r.Detail["internal"] = internal
	}
	return r
}

// ─── db_schema_dump ─────────────────────────────────────────────────

type dbSchemaIn struct {
	Repo   string `json:"repo,omitempty"`
	Engine string `json:"engine"         jsonschema:"mysql|mariadb|tidb|postgres|postgresql|mongodb|redis|elasticsearch"`
	DB     string `json:"db"`
}
type dbSchemaOut struct {
	Engine string         `json:"engine"`
	DB     string         `json:"db"`
	Schema map[string]any `json:"schema"`
}

func dbSchemaDumpTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in dbSchemaIn) (*mcpsdk.CallToolResult, dbSchemaOut, error) {
	cfg, err := loadCfgForRepo(in.Repo)
	if err != nil {
		return nil, dbSchemaOut{}, err
	}
	out := dbSchemaOut{Engine: in.Engine, DB: in.DB, Schema: map[string]any{}}
	var schema map[string]any
	fam, ok := engine.Canonical(in.Engine)
	if !ok {
		return nil, out, fmt.Errorf("unsupported engine: %s", in.Engine)
	}
	switch fam {
	case engine.FamilyMySQL:
		schema, err = dbSchemaMySQL(ctx, cfg, in.DB)
	case engine.FamilyPostgres:
		schema, err = dbSchemaPostgres(ctx, cfg, in.DB)
	case engine.FamilyMongo:
		schema, err = dbSchemaMongo(ctx, cfg, in.DB)
	case engine.FamilyES:
		schema, err = dbSchemaES(ctx, cfg, in.DB)
	case engine.FamilyRedis:
		schema, err = dbSchemaRedis(ctx, cfg, in.DB)
	}
	if err != nil {
		return nil, out, err
	}
	out.Schema = schema
	return nil, out, nil
}

func dbSchemaMySQL(ctx context.Context, cfg *config.Config, db string) (map[string]any, error) {
	if cfg.Connections.Mysql == nil {
		return nil, errors.New("connections.mysql not configured")
	}
	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		return nil, err
	}
	defer func() { _ = drv.Close() }()
	return mysqlSchema(ctx, drv, db)
}

func dbSchemaPostgres(ctx context.Context, cfg *config.Config, db string) (map[string]any, error) {
	if cfg.Connections.Postgres == nil {
		return nil, errors.New("connections.postgres not configured")
	}
	drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
	if err != nil {
		return nil, err
	}
	defer func() { _ = drv.Close() }()
	return postgresSchema(ctx, drv, db)
}

func dbSchemaMongo(ctx context.Context, cfg *config.Config, db string) (map[string]any, error) {
	if cfg.Connections.Mongodb == nil {
		return nil, errors.New("connections.mongodb not configured")
	}
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		return nil, err
	}
	defer func() { _ = drv.Close(ctx) }()
	return mongoSchema(ctx, drv, db)
}

func dbSchemaES(ctx context.Context, cfg *config.Config, db string) (map[string]any, error) {
	if cfg.Connections.Elasticsearch == nil {
		return nil, errors.New("connections.elasticsearch not configured")
	}
	drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
	if err != nil {
		return nil, err
	}
	return esSchema(ctx, drv, db)
}

func dbSchemaRedis(ctx context.Context, cfg *config.Config, db string) (map[string]any, error) {
	if cfg.Connections.Redis == nil {
		return nil, errors.New("connections.redis not configured")
	}
	drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
	if err != nil {
		return nil, err
	}
	defer func() { _ = drv.Close() }()
	return redisSchema(ctx, drv, db)
}

// ─── db_query ───────────────────────────────────────────────────────

type dbQueryIn struct {
	Repo       string `json:"repo,omitempty"`
	Engine     string `json:"engine"`
	DB         string `json:"db"`
	Query      string `json:"query,omitempty"       jsonschema:"SQL for mysql/postgres; raw command for redis"`
	Collection string `json:"collection,omitempty"  jsonschema:"required for mongodb"`
	FilterJSON string `json:"filter_json,omitempty" jsonschema:"mongodb filter document"`
	Index      string `json:"index,omitempty"       jsonschema:"elasticsearch index name (overrides db field)"`
	BodyJSON   string `json:"body_json,omitempty"   jsonschema:"elasticsearch _search body"`
	Limit      int    `json:"limit,omitempty"       jsonschema:"max rows/docs returned (default 100)"`
}
type dbQueryOut struct {
	Engine  string   `json:"engine"`
	Rows    []any    `json:"rows"`
	Columns []string `json:"columns,omitempty"`
}

func dbQueryTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in dbQueryIn) (*mcpsdk.CallToolResult, dbQueryOut, error) {
	if in.Limit <= 0 {
		in.Limit = 100
	}
	cfg, err := loadCfgForRepo(in.Repo)
	if err != nil {
		return nil, dbQueryOut{}, err
	}
	out := dbQueryOut{Engine: in.Engine}
	var rows []any
	var cols []string
	fam, ok := engine.Canonical(in.Engine)
	if !ok {
		return nil, out, fmt.Errorf("unsupported engine: %s", in.Engine)
	}
	switch fam {
	case engine.FamilyMySQL:
		rows, cols, err = dbQueryMySQL(ctx, cfg, in)
	case engine.FamilyPostgres:
		rows, cols, err = dbQueryPostgres(ctx, cfg, in)
	case engine.FamilyMongo:
		rows, err = dbQueryMongo(ctx, cfg, in)
	case engine.FamilyES:
		rows, err = dbQueryES(ctx, cfg, in)
	case engine.FamilyRedis:
		rows, err = dbQueryRedis(ctx, cfg, in)
	}
	if err != nil {
		return nil, out, err
	}
	out.Rows, out.Columns = rows, cols
	return nil, out, nil
}

func dbQueryMySQL(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, []string, error) {
	if err := assertReadOnlySQL(in.Query); err != nil {
		return nil, nil, err
	}
	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = drv.Close() }()
	qdb, err := ident.QuoteMySQL(in.DB)
	if err != nil {
		return nil, nil, fmt.Errorf("db %q: %w", in.DB, err)
	}
	if _, err := drv.DB.ExecContext(ctx, "USE "+qdb); err != nil {
		return nil, nil, fmt.Errorf("use %s: %w", in.DB, err)
	}
	return runSQLQuery(ctx, drv.DB, in.Query, in.Limit)
}

func dbQueryPostgres(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, []string, error) {
	if err := assertReadOnlySQL(in.Query); err != nil {
		return nil, nil, err
	}
	drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = drv.Close() }()
	scoped, err := drv.OpenScoped(ctx, in.DB)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = scoped.Close() }()
	return runSQLQuery(ctx, scoped, in.Query, in.Limit)
}

func dbQueryMongo(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, error) {
	if in.Collection == "" {
		return nil, errors.New("mongodb query: collection required")
	}
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		return nil, err
	}
	defer func() { _ = drv.Close(ctx) }()
	filter := bson.D{}
	if strings.TrimSpace(in.FilterJSON) != "" {
		if err := bson.UnmarshalExtJSON([]byte(in.FilterJSON), false, &filter); err != nil {
			return nil, fmt.Errorf("parse filter_json: %w", err)
		}
	}
	col := drv.Client.Database(in.DB).Collection(in.Collection)
	// Use limit via Find options.
	cur, err := col.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()
	var rows []any
	for cur.Next(ctx) {
		if len(rows) >= in.Limit {
			break
		}
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		rows = append(rows, doc)
	}
	return rows, nil
}

func dbQueryES(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, error) {
	drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
	if err != nil {
		return nil, err
	}
	idx := in.Index
	if idx == "" {
		idx = in.DB
	}
	body := strings.TrimSpace(in.BodyJSON)
	if body == "" {
		body = `{"query":{"match_all":{}},"size":` + strconv.Itoa(in.Limit) + `}`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		drv.Base+"/"+idx+"/_search", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := drv.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	buf, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("es _search: %s: %s", resp.Status, string(buf))
	}
	var parsed map[string]any
	if err := json.Unmarshal(buf, &parsed); err != nil {
		return nil, fmt.Errorf("parse es response: %w", err)
	}
	var rows []any
	if hits, ok := parsed["hits"].(map[string]any); ok {
		if arr, ok := hits["hits"].([]any); ok {
			for i, h := range arr {
				if i >= in.Limit {
					break
				}
				rows = append(rows, h)
			}
		}
	}
	return rows, nil
}

func dbQueryRedis(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, error) {
	if err := assertReadOnlyRedis(in.Query); err != nil {
		return nil, err
	}
	drv, err := dbredis.Connect(ctx, *cfg.Connections.Redis)
	if err != nil {
		return nil, err
	}
	defer func() { _ = drv.Close() }()
	args := strings.Fields(in.Query)
	anyArgs := make([]any, len(args))
	for i, a := range args {
		anyArgs[i] = a
	}
	c := drv.Client()
	res, err := c.Do(ctx, anyArgs...).Result()
	if err != nil {
		return nil, err
	}
	return []any{res}, nil
}

// ─── snapshot_inspect ───────────────────────────────────────────────

type snapshotInspectIn struct {
	Fingerprint string `json:"fingerprint,omitempty"`
	Engine      string `json:"engine,omitempty"`
	SourceDB    string `json:"source_db,omitempty"`
	Repo        string `json:"repo,omitempty"`
}
type snapshotInspectOut struct {
	Record           map[string]any `json:"record"`
	TemplateExists   bool           `json:"template_exists"`
	TemplateSizeKB   int64          `json:"template_size_kb,omitempty"`
	EngineVersionNow string         `json:"engine_version_now,omitempty"`
}

func snapshotInspectTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in snapshotInspectIn,
) (*mcpsdk.CallToolResult, snapshotInspectOut, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, snapshotInspectOut{}, err
	}
	defer func() { _ = st.Close() }()
	var rec *storeSnapshotRecord
	if in.Fingerprint != "" {
		rec, err = snapshotLookupByFingerprint(ctx, st, in.Fingerprint)
		if err != nil {
			return nil, snapshotInspectOut{}, err
		}
	} else {
		rec, err = snapshotLookupByEngineSource(ctx, st, in.Engine, in.SourceDB)
		if err != nil {
			return nil, snapshotInspectOut{}, err
		}
	}
	if rec == nil {
		return nil, snapshotInspectOut{}, errors.New("no snapshot row matches the supplied criteria")
	}
	out := snapshotInspectOut{Record: snapshotRecordToMap(rec)}

	cfg, err := loadCfgForRepo(in.Repo)
	if err == nil {
		out.TemplateExists, out.TemplateSizeKB, out.EngineVersionNow = probeTemplate(ctx, cfg, rec.Engine, rec.TemplateName)
	}
	return nil, out, nil
}

// ─── snapshot_drop ──────────────────────────────────────────────────

type snapshotDropIn struct {
	Fingerprint string `json:"fingerprint"`
	Repo        string `json:"repo,omitempty"`
}
type snapshotDropOut struct {
	Dropped       bool   `json:"dropped"`
	EngineDropped bool   `json:"engine_dropped"`
	EngineDropErr string `json:"engine_drop_err,omitempty"`
	Engine        string `json:"engine,omitempty"`
	Template      string `json:"template,omitempty"`
}

func snapshotDropTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in snapshotDropIn) (*mcpsdk.CallToolResult, snapshotDropOut, error) {
	if in.Fingerprint == "" {
		return nil, snapshotDropOut{}, errors.New("fingerprint required")
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, snapshotDropOut{}, err
	}
	defer func() { _ = st.Close() }()
	rec, err := snapshotLookupByFingerprint(ctx, st, in.Fingerprint)
	if err != nil {
		return nil, snapshotDropOut{}, err
	}
	if rec == nil {
		return nil, snapshotDropOut{}, fmt.Errorf("no snapshot with fingerprint %s", in.Fingerprint)
	}
	out := snapshotDropOut{Engine: rec.Engine, Template: rec.TemplateName}

	cfg, cfgErr := loadCfgForRepo(in.Repo)
	if cfgErr == nil {
		if dropErr := dropTemplate(ctx, cfg, rec.Engine, rec.TemplateName); dropErr != nil {
			out.EngineDropped = false
			out.EngineDropErr = dropErr.Error()
		} else {
			out.EngineDropped = true
		}
	} else {
		out.EngineDropErr = "load config: " + cfgErr.Error()
	}
	if err := st.DeleteSnapshot(ctx, in.Fingerprint); err != nil {
		return nil, out, fmt.Errorf("delete snapshot row: %w", err)
	}
	out.Dropped = true
	return nil, out, nil
}

// ─── hook_log_read ──────────────────────────────────────────────────

type hookLogReadIn struct {
	WorktreePath string `json:"worktree_path"`
	Phase        string `json:"phase"`
	GroupIdx     int    `json:"group_idx,omitempty"`
	MaxBytes     int    `json:"max_bytes,omitempty" jsonschema:"truncate to last N bytes (0 = full file)"`
}
type hookLogReadOut struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Body      string `json:"body"`
	Truncated bool   `json:"truncated"`
}

func hookLogReadTool(_ context.Context, _ *mcpsdk.CallToolRequest, in hookLogReadIn) (*mcpsdk.CallToolResult, hookLogReadOut, error) {
	if in.WorktreePath == "" || in.Phase == "" {
		return nil, hookLogReadOut{}, errors.New("worktree_path and phase required")
	}
	p := filepath.Join(in.WorktreePath, ".treeman-hooks",
		fmt.Sprintf("%s-%d.log", in.Phase, in.GroupIdx))
	info, err := os.Stat(p)
	if err != nil {
		return nil, hookLogReadOut{}, fmt.Errorf("stat %s: %w", p, err)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, hookLogReadOut{}, err
	}
	defer func() { _ = f.Close() }()
	out := hookLogReadOut{Path: p, SizeBytes: info.Size()}
	if in.MaxBytes > 0 && info.Size() > int64(in.MaxBytes) {
		if _, err := f.Seek(info.Size()-int64(in.MaxBytes), io.SeekStart); err != nil {
			return nil, out, err
		}
		out.Truncated = true
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return nil, out, err
	}
	out.Body = string(body)
	return nil, out, nil
}

// ─── db_dump ────────────────────────────────────────────────────────

type dbDumpIn struct {
	Repo      string `json:"repo,omitempty"`
	Engine    string `json:"engine"`
	DB        string `json:"db"`
	OutputDir string `json:"output_dir,omitempty"`
	Gzip      bool   `json:"gzip,omitempty"`
}
type dbDumpOut struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

func dbDumpTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in dbDumpIn) (*mcpsdk.CallToolResult, dbDumpOut, error) {
	cfg, err := loadCfgForRepo(in.Repo)
	if err != nil {
		return nil, dbDumpOut{}, err
	}
	outDir := in.OutputDir
	if outDir == "" {
		repoRoot, _ := resolveRepo(in.Repo)
		outDir = filepath.Join(repoRoot, "storage", "dumps")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, dbDumpOut{}, err
	}
	ts := time.Now().UTC().Format("20060102-150405")
	fam, ok := engine.Canonical(in.Engine)
	if !ok {
		return nil, dbDumpOut{}, fmt.Errorf("db_dump: unknown engine %q (allowed: %s)", in.Engine, engine.KnownList())
	}
	switch fam {
	case engine.FamilyMySQL:
		p := filepath.Join(outDir, fmt.Sprintf("%s-%s.sql", in.DB, ts))
		if in.Gzip {
			p += ".gz"
		}
		return runMysqldump(ctx, cfg.Connections.Mysql, in.DB, p, in.Gzip)
	case engine.FamilyPostgres:
		p := filepath.Join(outDir, fmt.Sprintf("%s-%s.sql", in.DB, ts))
		if in.Gzip {
			p += ".gz"
		}
		return runPgDump(ctx, cfg.Connections.Postgres, in.DB, p, in.Gzip)
	case engine.FamilyMongo:
		p := filepath.Join(outDir, fmt.Sprintf("%s-%s.archive", in.DB, ts))
		if in.Gzip {
			p += ".gz"
		}
		return runMongoDump(ctx, cfg.Connections.Mongodb, in.DB, p, in.Gzip)
	case engine.FamilyES:
		p := filepath.Join(outDir, fmt.Sprintf("%s-%s.ndjson", in.DB, ts))
		if in.Gzip {
			p += ".gz"
		}
		return runESDump(ctx, cfg.Connections.Elasticsearch, in.DB, p, in.Gzip)
	case engine.FamilyRedis:
		return nil, dbDumpOut{}, errors.New(
			"db_dump: redis dump is not implemented — redis cold-build uses a `seed:` step rather than a dump file, so there is no restore counterpart. If you need a redis snapshot, use redis-cli BGSAVE / SAVE manually and reference the resulting RDB out-of-band",
		)
	default:
		return nil, dbDumpOut{}, fmt.Errorf("db_dump: engine family %q has no dump runner", fam)
	}
}

// ─── helpers ────────────────────────────────────────────────────────

func loadCfgForRepo(repo string) (*config.Config, error) {
	repoRoot, err := resolveRepo(repo)
	if err != nil {
		return nil, err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// splitTreemanNamespaces partitions engine namespace names into the
// app-visible set and treeman-managed internals (snapshot templates +
// branch_scoped durable copies), so engine_status surfaces app data
// without burying it under treeman bookkeeping — which branch_scoped
// multiplies (one durable per branch). internalPrefixes are the leading
// markers treeman reserves: `_tm` covers `_tm_` templates and `_tmbs_`
// durables for name-scoped engines; ES indices use `tm_`/`tmbs_`.
func splitTreemanNamespaces(names []string, internalPrefixes ...string) (app, internal []string) {
	for _, n := range names {
		isInternal := false
		for _, p := range internalPrefixes {
			if strings.HasPrefix(n, p) {
				isInternal = true
				break
			}
		}
		if isInternal {
			internal = append(internal, n)
		} else {
			app = append(app, n)
		}
	}
	return app, internal
}

// databasesDetail builds the engine_status Detail map for a name-scoped
// engine's database list, separating treeman-managed internals out of
// the headline `databases` list. Name-scoped templates (`_tm_…`) and
// branch_scoped durables (`_tmbs_…`) both lead with `_tm`; real app DBs
// don't lead a name with an underscore, so the prefix is a safe signal.
func databasesDetail(names []string) map[string]any {
	app, internal := splitTreemanNamespaces(names, "_tm")
	d := map[string]any{"databases": app}
	if len(internal) > 0 {
		d["internal_count"] = len(internal)
		d["internal"] = internal
	}
	return d
}

// esInternalIndexRe matches treeman's Elasticsearch template + durable
// index names: `tm_<16hex>_…` (snapshot template) and `tmbs_<16hex>_…`
// (branch_scoped durable). ES forbids leading `_`, so treeman can't use
// the name-scoped `_tm` marker here. The 16-hex fingerprint segment is
// required so a legitimate app index like `tm_products` is NOT
// misclassified as internal.
var esInternalIndexRe = regexp.MustCompile(`^(tm|tmbs)_[0-9a-f]{16}_`)

// isESInternalIndex reports whether an ES index name is treeman-managed
// (template or branch_scoped durable) rather than app data.
func isESInternalIndex(name string) bool {
	return strings.HasPrefix(name, "_tm") || esInternalIndexRe.MatchString(name)
}

func listMysqlDatabases(ctx context.Context, drv *dbmysql.Driver) ([]string, error) {
	rows, err := drv.DB.QueryContext(ctx, `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('information_schema','mysql','performance_schema','sys')
		ORDER BY schema_name
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func listPostgresDatabases(ctx context.Context, drv *dbpostgres.Driver) ([]string, error) {
	rows, err := drv.DB.QueryContext(ctx, `
		SELECT datname FROM pg_database
		WHERE datistemplate = false AND datname NOT IN ('postgres')
		ORDER BY datname
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func mysqlSchema(ctx context.Context, drv *dbmysql.Driver, db string) (map[string]any, error) {
	qdb, err := ident.QuoteMySQL(db)
	if err != nil {
		return nil, fmt.Errorf("db %q: %w", db, err)
	}
	if _, err := drv.DB.ExecContext(ctx, "USE "+qdb); err != nil {
		return nil, fmt.Errorf("USE %s: %w", qdb, err)
	}
	rows, err := drv.DB.QueryContext(ctx,
		"SELECT TABLE_NAME FROM information_schema.tables WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME", db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tables := map[string]string{}
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, n := range names {
		qn, err := ident.QuoteMySQL(n)
		if err != nil {
			// Defensive: information_schema returned a name that
			// fails our identifier rules. Skip it rather than
			// inject — operator should investigate.
			continue
		}
		row := drv.DB.QueryRowContext(ctx, "SHOW CREATE TABLE "+qdb+"."+qn)
		var gotName, ddl string
		if err := row.Scan(&gotName, &ddl); err != nil {
			continue
		}
		tables[n] = ddl
	}
	return map[string]any{"tables": tables}, nil
}

func postgresSchema(ctx context.Context, drv *dbpostgres.Driver, db string) (map[string]any, error) {
	scoped, err := drv.OpenScoped(ctx, db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scoped.Close() }()
	rows, err := scoped.QueryContext(ctx, `
		SELECT table_schema, table_name
		FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog','information_schema')
		ORDER BY table_schema, table_name
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type tablePair struct{ schema, name string }
	var tables []tablePair
	for rows.Next() {
		var p tablePair
		_ = rows.Scan(&p.schema, &p.name)
		tables = append(tables, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	schema := map[string]any{}
	for _, t := range tables {
		cols, ok := postgresTableColumns(ctx, scoped, t.schema, t.name)
		if !ok {
			continue
		}
		schema[t.schema+"."+t.name] = cols
	}
	return map[string]any{"tables": schema}, nil
}

// postgresTableColumns reads one table's column metadata. ok is false
// when the column query fails, so the caller can omit the table from
// the schema map (a partial schema is better than aborting the whole
// dump for one unreadable table).
func postgresTableColumns(ctx context.Context, scoped *sql.DB, schema, name string) ([]map[string]any, bool) {
	colRows, err := scoped.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position
	`, schema, name)
	if err != nil {
		return nil, false
	}
	defer func() { _ = colRows.Close() }()
	var cols []map[string]any
	for colRows.Next() {
		var colName, dtype, nullable string
		var def sql.NullString
		_ = colRows.Scan(&colName, &dtype, &nullable, &def)
		cols = append(cols, map[string]any{
			"name": colName, "type": dtype,
			"nullable": nullable == "YES",
			"default":  def.String,
		})
	}
	if err := colRows.Err(); err != nil {
		return nil, false
	}
	return cols, true
}

func mongoSchema(ctx context.Context, drv *dbmongo.Driver, db string) (map[string]any, error) {
	cols, err := drv.Client.Database(db).ListCollectionNames(ctx, struct{}{})
	if err != nil {
		return nil, err
	}
	sort.Strings(cols)
	samples := map[string]any{}
	for _, c := range cols {
		var doc bson.M
		_ = drv.Client.Database(db).Collection(c).FindOne(ctx, struct{}{}).Decode(&doc)
		samples[c] = doc
	}
	return map[string]any{"collections": cols, "samples": samples}, nil
}

func esSchema(ctx context.Context, drv *dbes.Driver, indexPattern string) (map[string]any, error) {
	target := indexPattern
	if target == "" {
		target = "_all"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, drv.Base+"/"+target+"*/_mapping", nil)
	if err != nil {
		return nil, err
	}
	resp, err := drv.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	return map[string]any{"mappings": parsed}, nil
}

func redisSchema(ctx context.Context, drv *dbredis.Driver, prefix string) (map[string]any, error) {
	if prefix == "" {
		return nil, errors.New("redis schema_dump: prefix required (e.g. wt_feature-x:)")
	}
	c := drv.Client()
	var cursor uint64
	var keys []string
	for {
		batch, next, err := c.Scan(ctx, cursor, prefix+"*", 256).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 || len(keys) > 1000 {
			break
		}
	}
	types := map[string]int{}
	for _, k := range keys {
		t, err := c.Type(ctx, k).Result()
		if err == nil {
			types[t]++
		}
	}
	return map[string]any{"key_count": len(keys), "types": types}, nil
}

func assertReadOnlySQL(q string) error {
	q = strings.TrimSpace(strings.ToUpper(q))
	for _, prefix := range []string{"SELECT ", "SHOW ", "EXPLAIN ", "DESCRIBE ", "DESC ", "WITH "} {
		if strings.HasPrefix(q, prefix) {
			return nil
		}
	}
	return fmt.Errorf(
		"db_query (SQL) refuses non-read statements; got %q (must start with SELECT/SHOW/EXPLAIN/DESCRIBE/WITH)",
		firstWord(q),
	)
}

func assertReadOnlyRedis(q string) error {
	cmd := strings.ToUpper(firstWord(q))
	allowed := map[string]bool{
		"GET": true, "MGET": true, "SMEMBERS": true, "HGETALL": true,
		"KEYS": true, "SCAN": true, "EXISTS": true, "TYPE": true,
		"TTL": true, "PTTL": true, "LRANGE": true, "ZRANGE": true,
		"HKEYS": true, "HVALS": true, "HGET": true, "HMGET": true,
		"DBSIZE": true, "INFO": true, "PING": true,
	}
	if !allowed[cmd] {
		return fmt.Errorf("redis db_query refuses %q (allowed: GET/MGET/SMEMBERS/HGETALL/KEYS/SCAN/EXISTS/TYPE/TTL/LRANGE/...)", cmd)
	}
	return nil
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\n"); i > 0 {
		return s[:i]
	}
	return s
}

func runSQLQuery(ctx context.Context, db *sql.DB, q string, limit int) ([]any, []string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	out := []any{}
	for rows.Next() {
		if len(out) >= limit {
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, cols, err
		}
		row := map[string]any{}
		for i, c := range cols {
			row[c] = decodeSQLValue(vals[i])
		}
		out = append(out, row)
	}
	return out, cols, rows.Err()
}

func decodeSQLValue(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}
