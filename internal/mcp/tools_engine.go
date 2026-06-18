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
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	"github.com/stubbedev/treeman/internal/db/ident"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	dbs3 "github.com/stubbedev/treeman/internal/db/s3"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
)

// registerEngineTools binds the engine-introspection + snapshot
// inspection tools. Called from registerReadTools (read-only ones)
// and registerWriteTools (mutating ones).
func registerEngineReadTools(srv *mcpsdk.Server) {
	addTool(srv, &mcpsdk.Tool{
		Name:        "engine_status",
		Description: "Probe every engine in .treeman.yaml — reachable? version? per-DB summary. Use to answer \"are my databases up?\". Call before prepare_run/worktree_create against a fresh env, and as the second step in diagnose-prepare-failure.",
		Annotations: readOnlyAnno("Probe engine status", true),
	}, engineStatusTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "db_schema_dump",
		Description: "Dump the live schema for ONE database — mysql/postgres: CREATE TABLEs, mongo: collection list + samples, ES: index mapping, redis: SCAN-driven key summary. Use to reason about live shape vs. what migrations expect. db = the rendered per-worktree name (find via worktree_show or snapshots_inspect). Capped at max_tables (default 200).",
		Annotations: readOnlyAnno("Dump live schema", true),
	}, dbSchemaDumpTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "db_query",
		Description: "Read OR write a configured engine directly — no shelling out to mysql/psql/mongosh/redis-cli. Default write=false is READ-ONLY (SQL: SELECT/SHOW/EXPLAIN/DESCRIBE/WITH; Mongo: find-style filter JSON, or a read command_json; Redis: GET/MGET/SCAN/HGETALL/INFO/…; ES: _search body). Set write=true+ack=true to MUTATE: SQL DML/DDL (INSERT/UPDATE/DELETE/CREATE/ALTER/DROP, returns rows_affected), any Redis command, Mongo write commands via command_json ({\"insert\":…},{\"update\":…},{\"delete\":…}). ES writes use es_request. ALWAYS FILTER + PAGINATE reads: add a WHERE/filter to narrow rows, keep limit small (default 100), and when truncated=true is returned re-call with offset += limit (SQL: put your own LIMIT/OFFSET in the query) — never pull a whole table in one shot.",
		Annotations: writeAnno("Query or mutate engine", true, false, true),
	}, dbQueryTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "es_request",
		Description: "Direct Elasticsearch REST passthrough using the configured ES connection (auth handled) — replaces curl for any endpoint db_query doesn't cover: _cat/indices, _cluster/health, _mapping, _settings, _aliases, doc get/index, _bulk, _search. GET/HEAD + POST reads run freely; PUT/DELETE and writing POSTs (_doc/_bulk/_update) need write=true+ack=true. For reads that can return many hits, FILTER + PAGINATE in the body (a real query plus from/size, not match_all) instead of pulling everything; _cat endpoints accept ?format=json&s=… to sort/limit. Returns {status, body|text}.",
		Annotations: writeAnno("Elasticsearch REST", true, false, true),
	}, esRequestTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "snapshots_inspect",
		Description: "Inspect ONE snapshot (by fingerprint, or engine+source_db) — SQLite row + does the engine-side template still exist + size + engine version. Call before snapshots_drop. Diagnoses \"cache hit but prepare failed\": template_exists=false → orphan row, next prepare will (correctly) cold-build. For sweeps use the cache-cleanup prompt.",
		Annotations: readOnlyAnno("Inspect snapshot", true),
	}, snapshotInspectTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "hook_log_read",
		Description: "Read the FULL hook log file for one (worktree, phase, group_idx). logs_hooks only keeps 16KB tails — use this when you need more. max_bytes=N returns just the trailing N bytes.",
		Annotations: readOnlyAnno("Read hook log", false),
	}, hookLogReadTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "connection_probe",
		Description: "Dry-test a connection string against an engine without writing it to .treeman.yaml. Tightens the setup loop — iterate on credentials, observe reachable/version/latency, then commit via config_set. dsn forms: mysql://user:pw@host:port, postgres://…, mongodb://…, redis://…, http(s)://…(ES). Omit dsn to probe the repo's currently-configured connection.",
		Annotations: readOnlyAnno("Probe connection", true),
	}, connectionProbeTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "engine_logs",
		Description: "Tail the container logs for one configured engine — `docker logs --tail N --since S` (or podman/nerdctl/finch per the connection block's container_engine). Closes the \"why is MySQL refusing connections?\" gap so the agent doesn't have to leave MCP for the diagnosis. Errors with a clear message when the engine's connection block has no container/compose ref (raw host engines need host-side log access).",
		Annotations: readOnlyAnno("Tail engine logs", true),
	}, engineLogsTool)
}

func registerEngineWriteTools(srv *mcpsdk.Server) {
	addTool(srv, &mcpsdk.Tool{
		Name:        "snapshots_drop",
		Description: "Evict ONE snapshot by fingerprint (engine-side template + SQLite row). Next prepare cold-rebuilds. Call snapshots_inspect first to confirm. Engine-drop failures abort before the SQLite row is touched, so partial failures leave a recoverable state. Pass dry_run=true to preview, ack=true to skip confirmation. For orphan sweeps the cache-cleanup prompt is safer than calling this directly.",
		Annotations: writeAnno("Drop snapshot", true, true, true),
	}, snapshotDropTool)

	addTool(srv, &mcpsdk.Tool{
		Name:        "db_dump",
		Description: "Refresh the seed dump used for cold builds — runs mysqldump / pg_dump / mongodump / ES scroll-bulk. Commit the new file then trigger prepare_run. Redis intentionally not implemented (uses a seed step, not a dump). output_dir defaults to <repo>/storage/dumps.",
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
	// Probe every engine concurrently — each connect+version+list is an
	// independent network round-trip, so serial probing blocked to the
	// dial timeout of each unreachable engine in turn. Results are
	// written to distinct slice indices (no shared mutation) and the
	// fixed order is preserved.
	probes := []func(context.Context, *config.Config) engineProbeResult{
		probeMySQL, probePostgres, probeMongo, probeRedis, probeES, probeS3,
	}
	results := make([]engineProbeResult, len(probes))
	var wg sync.WaitGroup
	for i, probe := range probes {
		wg.Go(func() {
			results[i] = probe(ctx, cfg)
		})
	}
	wg.Wait()
	return nil, engineStatusOut{Engines: results}, nil
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

func probeS3(ctx context.Context, cfg *config.Config) engineProbeResult {
	r := engineProbeResult{Engine: "s3"}
	if cfg.Connections.S3 == nil {
		return r
	}
	r.Configured = true
	drv, err := dbs3.Connect(ctx, *cfg.Connections.S3)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Reachable = true
	r.Version, _ = drv.EngineVersion(ctx)

	// Restrict the listing to the literal prefix(es) carried by the
	// configured s3 databases. ListBuckets returns the whole AWS
	// account, so listing under "" would leak every unrelated bucket
	// the credentials can see into the MCP response.
	prefixes := s3LiteralPrefixes(cfg)
	seen := map[string]bool{}
	var buckets []string
	for _, p := range prefixes {
		matches, err := drv.ListMatching(ctx, p)
		if err != nil {
			r.Error = err.Error()
			return r
		}
		for _, b := range matches {
			if seen[b] {
				continue
			}
			seen[b] = true
			buckets = append(buckets, b)
		}
	}
	r.Detail = map[string]any{"buckets": buckets, "scanned_prefixes": prefixes}
	return r
}

// s3LiteralPrefixes returns the deduplicated literal portion (text
// before the first `{`) of every `engine: s3` entry's key_prefix.
// Used by probeS3 to scope ListBuckets so unrelated buckets in the
// same AWS account don't leak into MCP responses.
func s3LiteralPrefixes(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range cfg.Databases {
		if d.Engine != "s3" || d.KeyPrefix == "" {
			continue
		}
		lit := d.KeyPrefix
		if i := strings.Index(lit, "{"); i >= 0 {
			lit = lit[:i]
		}
		if lit == "" || seen[lit] {
			continue
		}
		seen[lit] = true
		out = append(out, lit)
	}
	return out
}

// ─── db_schema_dump ─────────────────────────────────────────────────

type dbSchemaIn struct {
	Repo      string `json:"repo,omitempty"`
	Engine    string `json:"engine"               jsonschema:"mysql|mariadb|tidb|postgres|postgresql|mongodb|redis|elasticsearch"`
	DB        string `json:"db"`
	MaxTables int    `json:"max_tables,omitempty" jsonschema:"sql/mongo: cap on the number of tables/collections returned (default 200). truncated=true is set in the output when the cap kicks in."`
}
type dbSchemaOut struct {
	Engine    string         `json:"engine"`
	DB        string         `json:"db"`
	Schema    map[string]any `json:"schema"`
	Truncated bool           `json:"truncated,omitempty"`
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
	case engine.FamilyS3:
		err = fmt.Errorf("engine %q is an object store — it has no schema to dump", in.Engine)
	}
	if err != nil {
		return nil, out, err
	}
	out.Schema = schema
	out.Truncated = capSchemaTables(out.Schema, in.MaxTables)
	return nil, out, nil
}

// capSchemaTables trims the per-table/per-collection maps in a schema
// payload to maxTables entries. mysql/postgres land in `tables`; mongo
// uses `collections` + `samples`. ES returns one nested `mappings`
// blob (no obvious per-key cap), redis already returns a count summary.
// Returns true when anything was dropped so the caller can flag truncated.
func capSchemaTables(schema map[string]any, maxTables int) bool {
	if maxTables <= 0 {
		maxTables = 200
	}
	truncated := false
	if tables, ok := schema["tables"].(map[string]any); ok && len(tables) > maxTables {
		keys := make([]string, 0, len(tables))
		for k := range tables {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		capped := map[string]any{}
		for _, k := range keys[:maxTables] {
			capped[k] = tables[k]
		}
		schema["tables"] = capped
		schema["tables_total"] = len(keys)
		truncated = true
	}
	if cols, ok := schema["collections"].([]string); ok && len(cols) > maxTables {
		schema["collections"] = cols[:maxTables]
		schema["collections_total"] = len(cols)
		if samples, ok := schema["samples"].(map[string]any); ok {
			kept := map[string]any{}
			for _, c := range cols[:maxTables] {
				if s, ok := samples[c]; ok {
					kept[c] = s
				}
			}
			schema["samples"] = kept
		}
		truncated = true
	}
	return truncated
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
	Repo        string `json:"repo,omitempty"`
	Engine      string `json:"engine"`
	DB          string `json:"db"`
	Query       string `json:"query,omitempty"        jsonschema:"SQL for mysql/postgres; raw command for redis"`
	Collection  string `json:"collection,omitempty"   jsonschema:"mongodb collection name (read/find path)"`
	FilterJSON  string `json:"filter_json,omitempty"  jsonschema:"mongodb filter document (read/find path)"`
	CommandJSON string `json:"command_json,omitempty" jsonschema:"mongodb: a raw command document run via RunCommand, e.g. {\"insert\":\"users\",\"documents\":[...]}, {\"aggregate\":\"c\",\"pipeline\":[...],\"cursor\":{}}, {\"count\":\"c\"}. Read commands run with write=false; write commands need write=true+ack."`
	Index       string `json:"index,omitempty"        jsonschema:"elasticsearch index name (overrides db field)"`
	BodyJSON    string `json:"body_json,omitempty"    jsonschema:"elasticsearch _search body (carry your own query/from/size for full control)"`
	Limit       int    `json:"limit,omitempty"        jsonschema:"PAGE SIZE — max rows/docs returned (default 100, hard cap). Keep small; never use this tool to dump a whole table — filter first, then page."`
	Offset      int    `json:"offset,omitempty"       jsonschema:"pagination offset (mongo skip / ES from). For SQL, put OFFSET in the query itself. Page by re-calling with offset += limit while truncated=true."`
	Write       bool   `json:"write,omitempty"        jsonschema:"allow MUTATIONS: SQL DML/DDL (INSERT/UPDATE/DELETE/CREATE/ALTER/DROP), any redis command, mongo write commands. Default false = read-only. Requires ack=true. ES writes go through es_request."`
	Ack         bool   `json:"ack,omitempty"          jsonschema:"required with write=true — confirms intent to mutate live engine data (irreversible)."`
}
type dbQueryOut struct {
	Engine    string   `json:"engine"`
	Wrote     bool     `json:"wrote,omitempty"`
	Rows      []any    `json:"rows"`
	Columns   []string `json:"columns,omitempty"`
	Affected  *int64   `json:"rows_affected,omitempty"`
	Truncated bool     `json:"truncated,omitempty"     jsonschema:"true when the page filled to limit — more rows likely exist; re-call with offset += limit (or a tighter filter)"`
}

func dbQueryTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in dbQueryIn) (*mcpsdk.CallToolResult, dbQueryOut, error) {
	if in.Limit <= 0 {
		in.Limit = 100
	}
	// Mutations are irreversible: write=true must be paired with an
	// explicit ack so an agent can't drop a table on a stray flag.
	if in.Write && !in.Ack {
		return nil, dbQueryOut{}, errors.New("write=true requires ack=true (you are about to mutate live engine data)")
	}
	cfg, err := loadCfgForRepo(in.Repo)
	if err != nil {
		return nil, dbQueryOut{}, err
	}
	out := dbQueryOut{Engine: in.Engine, Wrote: in.Write}
	var rows []any
	var cols []string
	var affected *int64
	fam, ok := engine.Canonical(in.Engine)
	if !ok {
		return nil, out, fmt.Errorf("unsupported engine: %s", in.Engine)
	}
	switch fam {
	case engine.FamilyMySQL:
		rows, cols, affected, err = dbQueryMySQL(ctx, cfg, in)
	case engine.FamilyPostgres:
		rows, cols, affected, err = dbQueryPostgres(ctx, cfg, in)
	case engine.FamilyMongo:
		rows, err = dbQueryMongo(ctx, cfg, in)
	case engine.FamilyES:
		rows, err = dbQueryES(ctx, cfg, in)
	case engine.FamilyRedis:
		rows, err = dbQueryRedis(ctx, cfg, in)
	case engine.FamilyS3:
		err = fmt.Errorf("engine %q is an object store — it is not queryable; use engine_status to inspect buckets", in.Engine)
	}
	if err != nil {
		return nil, out, err
	}
	out.Rows, out.Columns, out.Affected = rows, cols, affected
	// A full page signals there's likely more — nudge the agent to
	// paginate (offset += limit) or tighten the filter rather than
	// assume it has seen everything.
	if !in.Write && len(rows) >= in.Limit {
		out.Truncated = true
	}
	return nil, out, nil
}

func dbQueryMySQL(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, []string, *int64, error) {
	if !in.Write {
		if err := assertReadOnlySQL(in.Query); err != nil {
			return nil, nil, nil, err
		}
	}
	drv, err := dbmysql.Connect(ctx, *cfg.Connections.Mysql)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = drv.Close() }()
	qdb, err := ident.QuoteMySQL(in.DB)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("db %q: %w", in.DB, err)
	}
	if _, err := drv.DB.ExecContext(ctx, "USE "+qdb); err != nil {
		return nil, nil, nil, fmt.Errorf("use %s: %w", in.DB, err)
	}
	if in.Write {
		rows, aff, err := execSQL(ctx, drv.DB, in.Query)
		return rows, nil, aff, err
	}
	rows, cols, err := runSQLQuery(ctx, drv.DB, in.Query, in.Limit)
	return rows, cols, nil, err
}

func dbQueryPostgres(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, []string, *int64, error) {
	if !in.Write {
		if err := assertReadOnlySQL(in.Query); err != nil {
			return nil, nil, nil, err
		}
	}
	drv, err := dbpostgres.Connect(ctx, *cfg.Connections.Postgres)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = drv.Close() }()
	scoped, err := drv.OpenScoped(ctx, in.DB)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = scoped.Close() }()
	if in.Write {
		rows, aff, err := execSQL(ctx, scoped, in.Query)
		return rows, nil, aff, err
	}
	rows, cols, err := runSQLQuery(ctx, scoped, in.Query, in.Limit)
	return rows, cols, nil, err
}

// execSQL runs a mutating statement and reports rows-affected (plus the
// driver's last-insert-id when it exposes a useful one). The write path
// for db_query's SQL engines.
func execSQL(ctx context.Context, db *sql.DB, q string) ([]any, *int64, error) {
	res, err := db.ExecContext(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	aff, _ := res.RowsAffected()
	row := map[string]any{"rows_affected": aff}
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		row["last_insert_id"] = id
	}
	return []any{row}, &aff, nil
}

func dbQueryMongo(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, error) {
	// A command document gives full read+write access via RunCommand;
	// a bare collection+filter is the convenience read/find path.
	if strings.TrimSpace(in.CommandJSON) != "" {
		return dbCommandMongo(ctx, cfg, in)
	}
	if in.Write {
		return nil, errors.New(
			"mongodb write=true requires command_json (e.g. {\"insert\":\"coll\",\"documents\":[...]}); the collection+filter path is read-only find",
		)
	}
	if in.Collection == "" {
		return nil, errors.New("mongodb query: collection (or command_json) required")
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
	// Push limit + skip down to the server so pagination doesn't drag
	// the whole collection over the wire.
	opt := options.Find().SetLimit(int64(in.Limit))
	if in.Offset > 0 {
		opt = opt.SetSkip(int64(in.Offset))
	}
	cur, err := col.Find(ctx, filter, opt)
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

// mongoReadCommands is the set of RunCommand verbs that only read. A
// command_json whose first key isn't here is treated as a mutation and
// needs write=true+ack.
var mongoReadCommands = map[string]bool{
	"find": true, "aggregate": true, "count": true, "distinct": true,
	"listcollections": true, "listindexes": true, "dbstats": true,
	"collstats": true, "ping": true, "buildinfo": true, "serverstatus": true,
	"explain": true, "geosearch": true, "connectionstatus": true, "hello": true,
}

// dbCommandMongo runs a raw Mongo command via RunCommand — the unified
// read+write path. The command's first key decides read vs write: a read
// verb runs with write=false; anything else needs write=true+ack (already
// gated in dbQueryTool, re-checked here so write isn't silently downgraded).
func dbCommandMongo(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, error) {
	var cmd bson.D
	if err := bson.UnmarshalExtJSON([]byte(in.CommandJSON), false, &cmd); err != nil {
		return nil, fmt.Errorf("parse command_json: %w", err)
	}
	if len(cmd) == 0 {
		return nil, errors.New("command_json is empty")
	}
	verb := strings.ToLower(cmd[0].Key)
	if !mongoReadCommands[verb] && !in.Write {
		return nil, fmt.Errorf("mongo command %q is a write — set write=true and ack=true", cmd[0].Key)
	}
	drv, err := dbmongo.Connect(ctx, *cfg.Connections.Mongodb)
	if err != nil {
		return nil, err
	}
	defer func() { _ = drv.Close(ctx) }()
	var res bson.M
	if err := drv.Client.Database(in.DB).RunCommand(ctx, cmd).Decode(&res); err != nil {
		return nil, fmt.Errorf("run command: %w", err)
	}
	return []any{res}, nil
}

func dbQueryES(ctx context.Context, cfg *config.Config, in dbQueryIn) ([]any, error) {
	if in.Write {
		return nil, errors.New("elasticsearch writes go through the es_request tool (method=PUT/POST/DELETE), not db_query")
	}
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
		body = `{"query":{"match_all":{}},"from":` + strconv.Itoa(in.Offset) + `,"size":` + strconv.Itoa(in.Limit) + `}`
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
	if !in.Write {
		if err := assertReadOnlyRedis(in.Query); err != nil {
			return nil, err
		}
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

// ─── es_request ─────────────────────────────────────────────────────

type esRequestIn struct {
	Repo     string `json:"repo,omitempty"`
	Method   string `json:"method,omitempty"    jsonschema:"GET|HEAD|POST|PUT|DELETE (default GET)"`
	Path     string `json:"path"                jsonschema:"ES REST path, e.g. '_cat/indices?v', '_cluster/health', 'myindex/_mapping', 'myindex/_search', 'myindex/_doc/1', 'myindex/_bulk'. Leading slash optional."`
	BodyJSON string `json:"body_json,omitempty" jsonschema:"request body (JSON, or newline-delimited for _bulk)"`
	Write    bool   `json:"write,omitempty"     jsonschema:"required for mutating requests (PUT/DELETE, or POST to a write endpoint like _doc/_bulk/_update). GET/HEAD and POST reads (_search/_count) don't need it."`
	Ack      bool   `json:"ack,omitempty"       jsonschema:"required with write=true — confirms intent to mutate the live cluster (irreversible)."`
}
type esRequestOut struct {
	Status int    `json:"status"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Wrote  bool   `json:"wrote,omitempty"`
	Body   any    `json:"body,omitempty"`
	Text   string `json:"text,omitempty"  jsonschema:"raw response when not JSON (e.g. _cat plain-text output)"`
}

// esRequestTool is the direct Elasticsearch REST passthrough — it removes
// the need to shell out to curl for any endpoint db_query's _search path
// doesn't cover (_cat, _cluster, _mapping, _settings, _aliases, doc
// get/index, _bulk, …). It reuses the configured ES driver's
// authenticated HTTP client + base URL, so credentials never leave
// treeman. Reads (GET/HEAD, and POST to read endpoints) run freely;
// mutations require write=true+ack so an agent can't drop an index by
// accident.
func esRequestTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in esRequestIn) (*mcpsdk.CallToolResult, esRequestOut, error) {
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete:
	default:
		return nil, esRequestOut{}, fmt.Errorf("unsupported method %q (want GET|HEAD|POST|PUT|DELETE)", method)
	}
	path := strings.TrimPrefix(strings.TrimSpace(in.Path), "/")
	if path == "" {
		return nil, esRequestOut{}, errors.New("path is required")
	}
	// A non-read request is a mutation — gate it behind write+ack.
	if !esIsReadRequest(method, path) {
		if !in.Write {
			return nil, esRequestOut{}, fmt.Errorf("%s /%s looks like a write — set write=true (and ack=true) to run it", method, path)
		}
		if !in.Ack {
			return nil, esRequestOut{}, errors.New("write=true requires ack=true (you are about to mutate the live cluster)")
		}
	}
	cfg, err := loadCfgForRepo(in.Repo)
	if err != nil {
		return nil, esRequestOut{}, err
	}
	if cfg.Connections.Elasticsearch == nil {
		return nil, esRequestOut{}, errors.New("connections.elasticsearch not configured")
	}
	drv, err := dbes.Connect(ctx, *cfg.Connections.Elasticsearch)
	if err != nil {
		return nil, esRequestOut{}, err
	}
	var bodyReader io.Reader
	if b := strings.TrimSpace(in.BodyJSON); b != "" {
		bodyReader = strings.NewReader(in.BodyJSON)
	}
	req, err := http.NewRequestWithContext(ctx, method, drv.Base+"/"+path, bodyReader)
	if err != nil {
		return nil, esRequestOut{}, err
	}
	if bodyReader != nil {
		// _bulk wants ndjson; everything else is JSON. The header is
		// advisory for ES, but ndjson endpoints reject application/json.
		if strings.Contains(path, "_bulk") {
			req.Header.Set("Content-Type", "application/x-ndjson")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	resp, err := drv.HTTP.Do(req)
	if err != nil {
		return nil, esRequestOut{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	buf, _ := io.ReadAll(resp.Body)
	out := esRequestOut{Status: resp.StatusCode, Method: method, Path: path, Wrote: in.Write}
	var parsed any
	if json.Unmarshal(buf, &parsed) == nil {
		out.Body = parsed
	} else {
		out.Text = string(buf)
	}
	if resp.StatusCode >= 400 {
		return nil, out, fmt.Errorf("es %s /%s: %s", method, path, resp.Status)
	}
	if in.Write {
		writeMCPEvent(context.Background(), store.EvtMCPEsRequest, method+" /"+path, 0, map[string]string{
			"method": method,
			"path":   path,
		})
	}
	return nil, out, nil
}

// esIsReadRequest reports whether an ES REST call is non-mutating: any
// GET/HEAD, plus POST to the read endpoints (_search/_count/_mget/…) that
// conventionally use POST to carry a body. Everything else (PUT, DELETE,
// POST to _doc/_bulk/_update/etc.) is treated as a write.
func esIsReadRequest(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost:
		for _, ro := range []string{"_search", "_count", "_msearch", "_mget", "_field_caps", "_explain", "_validate", "_analyze", "_terms_enum", "_rank_eval"} {
			if strings.Contains(path, ro) {
				return true
			}
		}
	}
	return false
}

// ─── snapshots_inspect ───────────────────────────────────────────────

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

// ─── snapshots_drop ──────────────────────────────────────────────────

type snapshotDropIn struct {
	Fingerprint string `json:"fingerprint"`
	Repo        string `json:"repo,omitempty"`
	DryRun      bool   `json:"dry_run,omitempty" jsonschema:"plan only — resolve the snapshot row + report what would be dropped (engine, template name) without mutating SQLite or the engine"`
	Ack         bool   `json:"ack,omitempty"     jsonschema:"skip the elicitation confirmation prompt"`
}
type snapshotDropOut struct {
	Dropped       bool   `json:"dropped"`
	EngineDropped bool   `json:"engine_dropped"`
	EngineDropErr string `json:"engine_drop_err,omitempty"`
	Engine        string `json:"engine,omitempty"`
	Template      string `json:"template,omitempty"`
	DryRun        bool   `json:"dry_run,omitempty"`
	Refused       string `json:"refused,omitempty"`
}

// snapshotDropTool deletes ONE cached snapshot — engine-side template
// AND the SQLite row. Engine-drop failures abort BEFORE the SQLite
// delete so the agent never sees Dropped=true while an orphan template
// remains on the engine.
func snapshotDropTool(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	in snapshotDropIn,
) (*mcpsdk.CallToolResult, snapshotDropOut, error) {
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
	if in.DryRun {
		out.DryRun = true
		return nil, out, nil
	}
	if ok, reason := confirmDestructive(
		ctx,
		req,
		false,
		in.Ack,
		fmt.Sprintf(
			"Drop snapshot %s (engine=%s template=%s)? Engine template + SQLite row will be deleted.",
			in.Fingerprint,
			rec.Engine,
			rec.TemplateName,
		),
	); !ok {
		out.Refused = reason
		return nil, out, nil
	}

	cfg, cfgErr := loadCfgForRepo(in.Repo)
	if cfgErr != nil {
		out.EngineDropErr = "load config: " + cfgErr.Error()
		return nil, out, fmt.Errorf("load config: %w", cfgErr)
	}
	if dropErr := dropTemplate(ctx, cfg, rec.Engine, rec.TemplateName); dropErr != nil {
		// Don't proceed to the SQLite delete: leaving the row in
		// place AND the orphan template means the next snapshots_inspect
		// will surface the orphan correctly. Deleting the row here
		// would silently lose the link.
		out.EngineDropped = false
		out.EngineDropErr = dropErr.Error()
		return nil, out, fmt.Errorf("drop engine template: %w", dropErr)
	}
	out.EngineDropped = true
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

// ─── connection_probe ───────────────────────────────────────────────

type connectionProbeIn struct {
	Engine string `json:"engine"         jsonschema:"mysql|mariadb|tidb|postgres|postgresql|mongodb|redis|elasticsearch|opensearch"`
	DSN    string `json:"dsn,omitempty"  jsonschema:"connection string. mysql://user:pw@host:port, postgres://…, mongodb://…, redis://…, http(s)://…(ES). Omit to probe the repo's configured connection for this engine."`
	Repo   string `json:"repo,omitempty" jsonschema:"only used when dsn is empty — selects the .treeman.yaml whose connection block to probe"`
}

type connectionProbeOut struct {
	Engine    string `json:"engine"`
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
	Source    string `json:"source"            jsonschema:"dsn|config — which connection block was probed"`
	Error     string `json:"error,omitempty"`
}

// connectionProbeTool dials the supplied DSN (or the repo's configured
// connection when dsn is empty) and returns reachability + version +
// latency. The Connect call is timed so the agent can decide whether a
// slow handshake is the actual problem. Failures are reported as
// reachable=false rather than a tool error so the agent gets the error
// text without having to parse a transport-level failure.
func connectionProbeTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in connectionProbeIn,
) (*mcpsdk.CallToolResult, connectionProbeOut, error) {
	out := connectionProbeOut{Engine: strings.ToLower(in.Engine)}
	fam, ok := engine.Canonical(in.Engine)
	if !ok {
		out.Error = "unsupported engine: " + in.Engine
		return nil, out, nil
	}
	if in.DSN == "" {
		out.Source = "config"
	} else {
		out.Source = "dsn"
	}
	start := time.Now()
	reachable, version, err := probeConnection(ctx, fam, in.DSN, in.Repo)
	out.LatencyMs = time.Since(start).Milliseconds()
	out.Reachable = reachable
	out.Version = version
	if err != nil {
		// Redact in case the DSN itself contained credentials and the
		// engine echoed it back in the error.
		out.Error = redactSecrets(err.Error())
	}
	return nil, out, nil
}

// probeConnection dispatches on engine family. For DSN-supplied probes,
// builds an in-memory connection config; for config-supplied probes,
// loads the repo's configured block.
func probeConnection(ctx context.Context, fam engine.Family, dsn, repo string) (bool, string, error) {
	switch fam {
	case engine.FamilyMySQL:
		return probeMySQLConn(ctx, dsn, repo)
	case engine.FamilyPostgres:
		return probePostgresConn(ctx, dsn, repo)
	case engine.FamilyMongo:
		return probeMongoConn(ctx, dsn, repo)
	case engine.FamilyRedis:
		return probeRedisConn(ctx, dsn, repo)
	case engine.FamilyES:
		return probeESConn(ctx, dsn, repo)
	case engine.FamilyS3:
		// S3 carries no DSN scalar; its reachability is reported by the
		// engine_status probe (probeS3), which dials via the configured
		// connection block. Redirect rather than pretend support.
		return false, "", errors.New("s3 connectivity is reported via engine_status, not db_connect")
	}
	return false, "", fmt.Errorf("no probe handler for family %s", fam)
}

func probeMySQLConn(ctx context.Context, dsn, repo string) (bool, string, error) {
	var cfg config.MysqlConn
	if dsn != "" {
		if err := yamlScalar(&cfg, dsn); err != nil {
			return false, "", err
		}
	} else {
		c, err := loadCfgForRepo(repo)
		if err != nil {
			return false, "", err
		}
		if c.Connections.Mysql == nil {
			return false, "", errors.New("connections.mysql not configured")
		}
		cfg = *c.Connections.Mysql
	}
	drv, err := dbmysql.Connect(ctx, cfg)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = drv.Close() }()
	v, _ := drv.EngineVersion(ctx)
	return true, v, nil
}

func probePostgresConn(ctx context.Context, dsn, repo string) (bool, string, error) {
	var cfg config.PostgresConn
	if dsn != "" {
		if err := yamlScalar(&cfg, dsn); err != nil {
			return false, "", err
		}
	} else {
		c, err := loadCfgForRepo(repo)
		if err != nil {
			return false, "", err
		}
		if c.Connections.Postgres == nil {
			return false, "", errors.New("connections.postgres not configured")
		}
		cfg = *c.Connections.Postgres
	}
	drv, err := dbpostgres.Connect(ctx, cfg)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = drv.Close() }()
	v, _ := drv.EngineVersion(ctx)
	return true, v, nil
}

func probeMongoConn(ctx context.Context, dsn, repo string) (bool, string, error) {
	var cfg config.MongoConn
	if dsn != "" {
		cfg.URI = dsn
	} else {
		c, err := loadCfgForRepo(repo)
		if err != nil {
			return false, "", err
		}
		if c.Connections.Mongodb == nil {
			return false, "", errors.New("connections.mongodb not configured")
		}
		cfg = *c.Connections.Mongodb
	}
	drv, err := dbmongo.Connect(ctx, cfg)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = drv.Close(ctx) }()
	v, _ := drv.EngineVersion(ctx)
	return true, v, nil
}

func probeRedisConn(ctx context.Context, dsn, repo string) (bool, string, error) {
	var cfg config.RedisConn
	if dsn != "" {
		cfg.URL = dsn
	} else {
		c, err := loadCfgForRepo(repo)
		if err != nil {
			return false, "", err
		}
		if c.Connections.Redis == nil {
			return false, "", errors.New("connections.redis not configured")
		}
		cfg = *c.Connections.Redis
	}
	drv, err := dbredis.Connect(ctx, cfg)
	if err != nil {
		return false, "", err
	}
	defer func() { _ = drv.Close() }()
	v, _ := drv.EngineVersion(ctx)
	return true, v, nil
}

func probeESConn(ctx context.Context, dsn, repo string) (bool, string, error) {
	var cfg config.EsConn
	if dsn != "" {
		cfg.URL = dsn
	} else {
		c, err := loadCfgForRepo(repo)
		if err != nil {
			return false, "", err
		}
		if c.Connections.Elasticsearch == nil {
			return false, "", errors.New("connections.elasticsearch not configured")
		}
		cfg = *c.Connections.Elasticsearch
	}
	drv, err := dbes.Connect(ctx, cfg)
	if err != nil {
		return false, "", err
	}
	v, _ := drv.EngineVersion(ctx)
	return true, v, nil
}

// ─── engine_logs ────────────────────────────────────────────────────

type engineLogsIn struct {
	Repo   string `json:"repo,omitempty"`
	Engine string `json:"engine"          jsonschema:"mysql|mariadb|tidb|postgres|postgresql|mongodb|redis|elasticsearch|opensearch"`
	Lines  int    `json:"lines,omitempty" jsonschema:"max lines returned (default 200, max 5000)"`
	Since  string `json:"since,omitempty" jsonschema:"only logs newer than this — duration like 10m|2h (default empty = no since filter)"`
}

type engineLogsOut struct {
	Engine        string `json:"engine"`
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	EngineBinary  string `json:"engine_binary"            jsonschema:"docker|podman|nerdctl|finch — the binary used to run logs"`
	Lines         int    `json:"lines"`
	Body          string `json:"body"`
}

// engineLogsTool shells out to `<engine_binary> logs --tail N [--since S]`
// for the container backing one configured engine. Read-only — the
// engine's logs are stdout, not engine state. Refuses when the
// engine's connection block has no container_ref (raw host engines
// need out-of-band log access).
func engineLogsTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in engineLogsIn,
) (*mcpsdk.CallToolResult, engineLogsOut, error) {
	cfg, err := loadCfgForRepo(in.Repo)
	if err != nil {
		return nil, engineLogsOut{}, err
	}
	ref, err := containerRefForEngine(cfg, in.Engine)
	if err != nil {
		return nil, engineLogsOut{}, err
	}
	lines := in.Lines
	if lines <= 0 {
		lines = 200
	}
	if lines > 5000 {
		lines = 5000
	}
	bin := ref.ContainerEngine
	if bin == "" {
		bin = "docker"
	}
	id, err := containerip.ContainerID(ctx, containerip.Opts{
		Container:      ref.Container,
		ComposeService: ref.ComposeService,
		ComposeProject: ref.ComposeProject,
		Engine:         bin,
	})
	if err != nil {
		return nil, engineLogsOut{}, fmt.Errorf("resolve container: %w", err)
	}
	args := []string{"logs", "--tail", strconv.Itoa(lines)}
	if in.Since != "" {
		args = append(args, "--since", in.Since)
	}
	args = append(args, id)
	cmd := exec.CommandContext(ctx, bin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, engineLogsOut{}, fmt.Errorf("%s logs: %w (output: %s)", bin, err, string(output))
	}
	body := redactSecrets(string(output))
	name := ref.Container
	if name == "" {
		name = ref.ComposeService
	}
	return nil, engineLogsOut{
		Engine:        strings.ToLower(in.Engine),
		ContainerID:   id,
		ContainerName: name,
		EngineBinary:  bin,
		Lines:         strings.Count(body, "\n"),
		Body:          body,
	}, nil
}

// containerRefForEngine returns the ContainerRef from the engine's
// connection block. Errors when the connection isn't configured or
// has no container/compose ref — agents need an explicit signal
// for "this engine isn't containerised" so they can suggest host
// logs instead.
func containerRefForEngine(cfg *config.Config, eng string) (*config.ContainerRef, error) {
	fam, ok := engine.Canonical(eng)
	if !ok {
		return nil, fmt.Errorf("unsupported engine: %s", eng)
	}
	var ref *config.ContainerRef
	switch fam {
	case engine.FamilyMySQL:
		if cfg.Connections.Mysql == nil {
			return nil, errors.New("connections.mysql not configured")
		}
		ref = &cfg.Connections.Mysql.ContainerRef
	case engine.FamilyPostgres:
		if cfg.Connections.Postgres == nil {
			return nil, errors.New("connections.postgres not configured")
		}
		ref = &cfg.Connections.Postgres.ContainerRef
	case engine.FamilyMongo:
		if cfg.Connections.Mongodb == nil {
			return nil, errors.New("connections.mongodb not configured")
		}
		ref = &cfg.Connections.Mongodb.ContainerRef
	case engine.FamilyRedis:
		if cfg.Connections.Redis == nil {
			return nil, errors.New("connections.redis not configured")
		}
		ref = &cfg.Connections.Redis.ContainerRef
	case engine.FamilyES:
		if cfg.Connections.Elasticsearch == nil {
			return nil, errors.New("connections.elasticsearch not configured")
		}
		ref = &cfg.Connections.Elasticsearch.ContainerRef
	case engine.FamilyS3:
		if cfg.Connections.S3 == nil {
			return nil, errors.New("connections.s3 not configured")
		}
		ref = &cfg.Connections.S3.ContainerRef
	}
	if ref == nil || (ref.Container == "" && ref.ComposeService == "") {
		return nil, fmt.Errorf("engine %s has no container/compose ref — check the host engine logs directly", eng)
	}
	return ref, nil
}

// yamlScalar fills a config struct from a bare DSN scalar by routing
// through its UnmarshalYAML (where the DSN-vs-mapping logic lives).
// Marshalling the string and re-unmarshalling avoids duplicating each
// engine's DSN parser here.
func yamlScalar(v any, dsn string) error {
	b, err := yaml.Marshal(dsn)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, v)
}
