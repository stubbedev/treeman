// Package resolve fills in connection details from layered sources:
//
//  1. Explicit `connections.*` in `.treeman.yaml`
//  2. Per-engine URL env vars (MYSQL_URL, POSTGRES_URL/PG_URL,
//     MONGODB_URI/MONGO_URL, REDIS_URL, ELASTICSEARCH_URL, etc.)
//  3. Generic DATABASE_URL when the scheme matches
//  4. Repo env files — .env, .env.testing, .env.test, .env.local,
//     .env.testing.local (Laravel + Symfony + dotenv conventions).
//     Reads DB_HOST / DB_PORT / DB_USERNAME / DB_PASSWORD plus the
//     DB_TEST_* overrides Laravel uses. Spring Boot
//     (SPRING_DATASOURCE_*, SPRING_DATA_MONGODB_*, SPRING_DATA_REDIS_*),
//     Django (ELASTICSEARCH_DSL_HOSTS), Symfony aliases.
package resolve

import (
	"context"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/envfile"
)

// Source labels the provenance of a resolved connection.
type Source struct {
	Kind  SourceKind
	Label string // env-var name or file path; empty for Yaml/Default
}

// SourceKind enumerates the layers.
type SourceKind int

const (
	SourceYaml SourceKind = iota
	SourceEnvURL
	SourceDatabaseURL
	SourceRepoEnvFile
	SourceDefault
)

// String renders a label for `treeman config show --resolved`.
func (s Source) String() string {
	switch s.Kind {
	case SourceYaml:
		return "yaml"
	case SourceEnvURL:
		return "env:" + s.Label
	case SourceDatabaseURL:
		return "env:DATABASE_URL"
	case SourceRepoEnvFile:
		return "file:" + s.Label
	default:
		return "default"
	}
}

// Resolved bundles each engine's resolved connection + provenance.
type Resolved struct {
	Mysql         *resolvedConn[config.MysqlConn]
	Postgres      *resolvedConn[config.PostgresConn]
	Mongodb       *resolvedConn[config.MongoConn]
	Redis         *resolvedConn[config.RedisConn]
	Elasticsearch *resolvedConn[config.EsConn]
	S3            *resolvedConn[config.S3Conn]
}

type resolvedConn[T any] struct {
	Conn   T
	Source Source
}

// Resolve walks the layers and returns every connection that has a
// non-empty value. When `cfg.EnvSources` is set, those paths are
// read in order (last wins); otherwise the default search order is
// used.
func Resolve(cfg *config.Config, repoRoot string) Resolved {
	return resolveRoots(cfg, repoRoot)
}

// resolveRoots resolves against env_sources layered across several
// roots: earlier roots are the base, later roots override (per-file
// last-wins, see envfile.ReadLayered). Lets a worktree's own env files
// shadow the main checkout's while the main files fill any gap — a
// fresh worktree's `.env` copy only lands mid-finalize
// (worktrees.copies), so resolving against the worktree root alone
// would read nothing and `$NAME` refs would go empty.
func resolveRoots(cfg *config.Config, roots ...string) Resolved {
	env := loadRepoEnvRoots(roots, cfg.EnvSources)
	return Resolved{
		Mysql:         resolveMysql(cfg, env),
		Postgres:      resolvePostgres(cfg, env),
		Mongodb:       resolveMongodb(cfg, env),
		Redis:         resolveRedis(cfg, env),
		Elasticsearch: resolveElasticsearch(cfg, env),
		S3:            resolveS3(cfg, env),
	}
}

// ApplyEnvCredentials fills `cfg.Connections.*` in-place. Used by
// `LoadLayered` so the daemon + CLI both see a fully-resolved
// config without each having to call Resolve themselves.
//
// Last-resort fallback: when a connection has a ContainerRef set
// but its password is still empty after env-file resolution, look
// up the container's own Config.Env (`docker inspect ...`) and
// pick the conventional image variable (MYSQL_ROOT_PASSWORD,
// POSTGRES_PASSWORD, ...). Saves users from duplicating
// container secrets into a separate .env.
func ApplyEnvCredentials(cfg *config.Config, roots ...string) {
	r := resolveRoots(cfg, roots...)
	if r.Mysql != nil {
		v := r.Mysql.Conn
		cfg.Connections.Mysql = &v
	}
	if r.Postgres != nil {
		v := r.Postgres.Conn
		cfg.Connections.Postgres = &v
	}
	if r.Mongodb != nil {
		v := r.Mongodb.Conn
		cfg.Connections.Mongodb = &v
	}
	if r.Redis != nil {
		v := r.Redis.Conn
		cfg.Connections.Redis = &v
	}
	if r.Elasticsearch != nil {
		v := r.Elasticsearch.Conn
		cfg.Connections.Elasticsearch = &v
	}
	if r.S3 != nil {
		v := r.S3.Conn
		cfg.Connections.S3 = &v
	}
	fillFromContainerEnv(cfg)
}

// fillFromContainerEnv populates empty passwords by inspecting the
// referenced container's env. Best-effort: any error is swallowed so
// a missing engine binary / unreachable daemon doesn't break config
// loading. Drivers will surface the real connectivity error later.
func fillFromContainerEnv(cfg *config.Config) {
	if m := cfg.Connections.Mysql; m != nil && m.Password == "" && (m.Container != "" || m.ComposeService != "") {
		// No ctx available here: fillFromContainerEnv is reached via the
		// cached LoadResolved loaders which have no ctx, and threading one
		// through every config-load call site is out of scope.
		env, _ := containerip.EnvLookup(context.Background(), containerip.Opts{
			Container:      m.Container,
			ComposeService: m.ComposeService,
			ComposeProject: m.ComposeProject,
			Engine:         m.ContainerEngine,
		})
		for _, k := range []string{"MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD", "MYSQL_PASSWORD"} {
			if v, ok := env[k]; ok && nonEmpty(v) {
				m.Password = v
				break
			}
		}
	}
	if p := cfg.Connections.Postgres; p != nil && p.Password == "" && (p.Container != "" || p.ComposeService != "") {
		env, _ := containerip.EnvLookup(context.Background(), containerip.Opts{
			Container:      p.Container,
			ComposeService: p.ComposeService,
			ComposeProject: p.ComposeProject,
			Engine:         p.ContainerEngine,
		})
		for _, k := range []string{"POSTGRES_PASSWORD", "POSTGRESQL_PASSWORD"} {
			if v, ok := env[k]; ok && nonEmpty(v) {
				p.Password = v
				break
			}
		}
	}
	fillS3FromContainerEnv(cfg.Connections.S3)
}

// fillS3FromContainerEnv back-fills S3 access/secret keys from the
// container's own environment when they weren't supplied in config —
// MinIO/Garage publish their root creds as well-known env vars
// (MINIO_ROOT_USER/PASSWORD, AWS_ACCESS_KEY_ID/SECRET_ACCESS_KEY), so a
// container-referenced block can authenticate with zero secret wiring.
func fillS3FromContainerEnv(s *config.S3Conn) {
	if s == nil || (s.Container == "" && s.ComposeService == "") {
		return
	}
	env, _ := containerip.EnvLookup(context.Background(), containerip.Opts{
		Container:      s.Container,
		ComposeService: s.ComposeService,
		ComposeProject: s.ComposeProject,
		Engine:         s.ContainerEngine,
	})
	if s.AccessKey == "" {
		for _, k := range []string{"MINIO_ROOT_USER", "MINIO_ACCESS_KEY", "AWS_ACCESS_KEY_ID"} {
			if v, ok := env[k]; ok && nonEmpty(v) {
				s.AccessKey = v
				break
			}
		}
	}
	if s.SecretKey == "" {
		for _, k := range []string{"MINIO_ROOT_PASSWORD", "MINIO_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"} {
			if v, ok := env[k]; ok && nonEmpty(v) {
				s.SecretKey = v
				break
			}
		}
	}
}

// loadRepoEnvRoots reads exactly the env files declared in
// `env_sources` (last layer wins), layered across every root in
// order: all of root[0]'s files first, then root[1]'s, … — so a later
// root overrides an earlier one. When `sources` is empty the resolver
// reads nothing — no hidden default; `treeman init` emits a list
// tailored to the detected framework. Relative entries resolve
// against each root; absolute entries are honoured as-is and read
// once regardless of root count.
func loadRepoEnvRoots(roots []string, sources []string) envfile.EnvFile {
	return envfile.ReadLayered(envSourcePaths(roots, sources))
}

// envSourcePaths expands the env_sources list across every root in
// order (see loadRepoEnvRoots). Also consumed by the resolve cache to
// fingerprint the env files that fed a cached resolution.
func envSourcePaths(roots []string, sources []string) []string {
	if len(sources) == 0 {
		return nil
	}
	paths := make([]string, 0, len(sources)*len(roots))
	seen := map[string]bool{}
	for _, root := range roots {
		for _, s := range sources {
			p := s
			if !filepath.IsAbs(s) {
				if root == "" {
					continue
				}
				p = filepath.Join(root, s)
			}
			if seen[p] {
				continue
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths
}

// ─────────────────────────── mysql ───────────────────────────

func resolveMysql(cfg *config.Config, env envfile.EnvFile) *resolvedConn[config.MysqlConn] {
	if cfg.Connections.Mysql != nil {
		m := *cfg.Connections.Mysql
		m.Password = resolveMysqlPassword(env, m.Password)
		return &resolvedConn[config.MysqlConn]{Conn: m, Source: Source{Kind: SourceYaml}}
	}
	// Spring Boot — SPRING_DATASOURCE_URL is JDBC-style; strip the
	// jdbc: prefix and parse.
	if m, ok := springMysql(env); ok {
		return &resolvedConn[config.MysqlConn]{Conn: m, Source: repoSrc(env)}
	}
	if u, src, ok := pickURL(env, "MYSQL_URL", []string{"mysql", "mariadb", "tidb"}); ok {
		m := mysqlFromURL(u)
		if p, ok := urlPassword(u); ok {
			m.Password = p
		}
		return &resolvedConn[config.MysqlConn]{Conn: m, Source: src}
	}
	// Laravel DB_TEST_* / DB_*.
	host := firstEnv(env, "DB_TEST_HOST", "DB_HOST")
	if host == "" {
		return nil
	}
	port := uint16(3306)
	if v := firstEnv(env, "DB_TEST_PORT", "DB_PORT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			port = uint16(n)
		}
	}
	user := firstEnv(env, "DB_TEST_USERNAME", "DB_USERNAME")
	if user == "" {
		user = "root"
	}
	return &resolvedConn[config.MysqlConn]{
		Conn: config.MysqlConn{
			Host:     host,
			Port:     port,
			User:     user,
			Password: resolveMysqlPassword(env, ""),
			PoolMax:  8,
		},
		Source: repoSrc(env),
	}
}

// springMysql parses SPRING_DATASOURCE_URL as a JDBC MySQL/MariaDB/TiDB
// connection, overlaying SPRING_DATASOURCE_PASSWORD/USERNAME and any
// URL-embedded password. Returns ok=false when the env var is absent or
// the JDBC engine is not a MySQL dialect.
func springMysql(env envfile.EnvFile) (config.MysqlConn, bool) {
	v, ok := env.Get("SPRING_DATASOURCE_URL")
	if !ok {
		return config.MysqlConn{}, false
	}
	u, eng := parseJDBC(v)
	if eng != "mysql" && eng != "mariadb" && eng != "tidb" {
		return config.MysqlConn{}, false
	}
	m := mysqlFromURL(u)
	if pw, ok := springPassword(env, u); ok {
		m.Password = pw
	}
	if user, ok := env.Get("SPRING_DATASOURCE_USERNAME"); ok {
		m.User = user
	}
	return m, true
}

func resolveMysqlPassword(env envfile.EnvFile, configured string) string {
	if v := resolvePasswordValue(env, configured); v != "" {
		return v
	}
	for _, k := range []string{"DB_TEST_PASSWORD", "DB_PASSWORD", "MYSQL_PASSWORD", "MYSQL_PWD"} {
		if v, ok := env.Get(k); ok && nonEmpty(v) {
			return v
		}
	}
	return ""
}

// ─────────────────────────── postgres ───────────────────────────

func resolvePostgres(cfg *config.Config, env envfile.EnvFile) *resolvedConn[config.PostgresConn] {
	if cfg.Connections.Postgres != nil {
		p := *cfg.Connections.Postgres
		p.Password = resolvePostgresPassword(env, p.Password)
		return &resolvedConn[config.PostgresConn]{Conn: p, Source: Source{Kind: SourceYaml}}
	}
	if p, ok := springPostgres(env); ok {
		return &resolvedConn[config.PostgresConn]{Conn: p, Source: repoSrc(env)}
	}
	if u, src, ok := pickURL(env, "POSTGRES_URL", []string{"postgres", "postgresql", "cockroach", "cockroachdb"}); ok {
		p := postgresFromURL(u)
		if pw, ok := urlPassword(u); ok {
			p.Password = pw
		}
		return &resolvedConn[config.PostgresConn]{Conn: p, Source: src}
	}
	if u, src, ok := pickURL(env, "PG_URL", []string{"postgres", "postgresql", "cockroach", "cockroachdb"}); ok {
		p := postgresFromURL(u)
		if pw, ok := urlPassword(u); ok {
			p.Password = pw
		}
		return &resolvedConn[config.PostgresConn]{Conn: p, Source: src}
	}
	return nil
}

// springPostgres parses SPRING_DATASOURCE_URL as a JDBC Postgres/Cockroach
// connection, overlaying SPRING_DATASOURCE_PASSWORD/USERNAME and any
// URL-embedded password. Returns ok=false when the env var is absent or
// the JDBC engine is not a Postgres dialect.
func springPostgres(env envfile.EnvFile) (config.PostgresConn, bool) {
	v, ok := env.Get("SPRING_DATASOURCE_URL")
	if !ok {
		return config.PostgresConn{}, false
	}
	u, eng := parseJDBC(v)
	if eng != "postgres" && eng != "cockroach" {
		return config.PostgresConn{}, false
	}
	p := postgresFromURL(u)
	if pw, ok := springPassword(env, u); ok {
		p.Password = pw
	}
	if user, ok := env.Get("SPRING_DATASOURCE_USERNAME"); ok {
		p.User = user
	}
	return p, true
}

func resolvePostgresPassword(env envfile.EnvFile, configured string) string {
	if v := resolvePasswordValue(env, configured); v != "" {
		return v
	}
	for _, k := range []string{"DB_TEST_PASSWORD", "DB_PASSWORD", "PGPASSWORD", "POSTGRES_PASSWORD"} {
		if v, ok := env.Get(k); ok && nonEmpty(v) {
			return v
		}
	}
	return ""
}

// resolvePasswordValue accepts a YAML-configured password that's
// either a literal value or an env-var reference. Recognised ref
// forms:
//
//	$NAME      — env var lookup. NAME must start with [A-Za-z_] and
//	             continue with [A-Za-z0-9_].
//	${NAME}    — same, with explicit braces (useful when the literal
//	             might continue with letters/digits).
//
// Empty input returns empty. Literal values (anything not matching
// the ref grammar) pass through unchanged — including literals that
// start with `$` but aren't a valid ref form (e.g. `$pass#word`).
func resolvePasswordValue(env envfile.EnvFile, configured string) string {
	if configured == "" {
		return ""
	}
	name := parsePasswordRef(configured)
	if name == "" {
		return configured
	}
	if v, ok := env.Get(name); ok && nonEmpty(v) {
		return v
	}
	return ""
}

// parsePasswordRef returns the env-var name if `s` is exactly
// `$NAME` or `${NAME}`. Returns "" if `s` is a literal.
func parsePasswordRef(s string) string {
	if len(s) < 2 || s[0] != '$' {
		return ""
	}
	if s[1] == '{' {
		if s[len(s)-1] != '}' {
			return ""
		}
		name := s[2 : len(s)-1]
		if !isIdent(name) {
			return ""
		}
		return name
	}
	name := s[1:]
	if !isIdent(name) {
		return ""
	}
	return name
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ─────────────────────────── mongo ───────────────────────────

func resolveMongodb(cfg *config.Config, env envfile.EnvFile) *resolvedConn[config.MongoConn] {
	if cfg.Connections.Mongodb != nil {
		m := *cfg.Connections.Mongodb
		// Whole-field `$NAME` ref, same contract as S3 fields.
		m.URI = resolvePasswordValue(env, m.URI)
		return &resolvedConn[config.MongoConn]{Conn: m, Source: Source{Kind: SourceYaml}}
	}
	for _, k := range []string{"MONGODB_URI", "MONGO_URL", "MONGO_URI", "MONGODB_URL", "SPRING_DATA_MONGODB_URI"} {
		if v, ok := env.Get(k); ok {
			return &resolvedConn[config.MongoConn]{Conn: config.MongoConn{URI: v}, Source: repoSrc(env)}
		}
	}
	// Spring Boot — compose from componentized props.
	if host, ok := env.Get("SPRING_DATA_MONGODB_HOST"); ok {
		port := envOr(env, "SPRING_DATA_MONGODB_PORT", "27017")
		user, _ := env.Get("SPRING_DATA_MONGODB_USERNAME")
		pass, _ := env.Get("SPRING_DATA_MONGODB_PASSWORD")
		userinfo := buildUserInfo(user, pass)
		db := ""
		if d, ok := env.Get("SPRING_DATA_MONGODB_DATABASE"); ok {
			db = "/" + d
		}
		return &resolvedConn[config.MongoConn]{
			Conn:   config.MongoConn{URI: "mongodb://" + userinfo + host + ":" + port + db},
			Source: repoSrc(env),
		}
	}
	// Laravel / generic.
	if host := firstEnv(env, "MONGO_DB_HOST", "MONGO_HOST"); host != "" {
		port := envOr(env, "MONGO_DB_PORT", envOr(env, "MONGO_PORT", "27017"))
		user := firstEnv(env, "MONGO_DB_USERNAME", "MONGO_USERNAME")
		pass := firstEnv(env, "MONGO_DB_PASSWORD", "MONGO_PASSWORD")
		userinfo := buildUserInfo(user, pass)
		return &resolvedConn[config.MongoConn]{
			Conn:   config.MongoConn{URI: "mongodb://" + userinfo + host + ":" + port},
			Source: repoSrc(env),
		}
	}
	return nil
}

// ─────────────────────────── redis ───────────────────────────

func resolveRedis(cfg *config.Config, env envfile.EnvFile) *resolvedConn[config.RedisConn] {
	if cfg.Connections.Redis != nil {
		r := *cfg.Connections.Redis
		r.URL = resolvePasswordValue(env, r.URL)
		return &resolvedConn[config.RedisConn]{Conn: r, Source: Source{Kind: SourceYaml}}
	}
	if v, ok := env.Get("REDIS_URL"); ok {
		return &resolvedConn[config.RedisConn]{Conn: config.RedisConn{URL: v}, Source: repoSrc(env)}
	}
	// Spring Boot.
	if host := firstEnv(env, "SPRING_DATA_REDIS_HOST", "SPRING_REDIS_HOST"); host != "" {
		port := firstEnvOr(env, "6379", "SPRING_DATA_REDIS_PORT", "SPRING_REDIS_PORT")
		pass := firstEnv(env, "SPRING_DATA_REDIS_PASSWORD", "SPRING_REDIS_PASSWORD")
		userinfo := ""
		if nonEmpty(pass) {
			userinfo = ":" + url.QueryEscape(pass) + "@"
		}
		return &resolvedConn[config.RedisConn]{
			Conn:   config.RedisConn{URL: "redis://" + userinfo + host + ":" + port},
			Source: repoSrc(env),
		}
	}
	// Laravel.
	if host, ok := env.Get("REDIS_HOST"); ok {
		port := envOr(env, "REDIS_PORT", "6379")
		pass, _ := env.Get("REDIS_PASSWORD")
		userinfo := ""
		if nonEmpty(pass) {
			userinfo = ":" + url.QueryEscape(pass) + "@"
		}
		return &resolvedConn[config.RedisConn]{
			Conn:   config.RedisConn{URL: "redis://" + userinfo + host + ":" + port},
			Source: repoSrc(env),
		}
	}
	return nil
}

// ─────────────────────────── elasticsearch ───────────────────────────

func resolveElasticsearch(cfg *config.Config, env envfile.EnvFile) *resolvedConn[config.EsConn] {
	if cfg.Connections.Elasticsearch != nil {
		e := *cfg.Connections.Elasticsearch
		e.URL = resolvePasswordValue(env, e.URL)
		return &resolvedConn[config.EsConn]{Conn: e, Source: Source{Kind: SourceYaml}}
	}
	for _, k := range []string{"ELASTICSEARCH_URL", "ELASTIC_URL", "OPENSEARCH_URL", "SPRING_ELASTICSEARCH_URIS"} {
		if v, ok := env.Get(k); ok {
			first := strings.SplitN(v, ",", 2)[0]
			first = strings.TrimSpace(first)
			if first == "" {
				continue
			}
			if !strings.Contains(first, "://") {
				first = "http://" + first
			}
			return &resolvedConn[config.EsConn]{Conn: config.EsConn{URL: first}, Source: repoSrc(env)}
		}
	}
	for _, k := range []string{"ELASTICSEARCH_HOSTS", "ELASTIC_HOSTS", "ES_HOSTS", "ELASTICSEARCH_DSL_HOSTS"} {
		if v, ok := env.Get(k); ok {
			first := strings.SplitN(v, ",", 2)[0]
			first = strings.TrimSpace(first)
			if first == "" {
				continue
			}
			if !strings.Contains(first, "://") {
				first = "http://" + first
			}
			return &resolvedConn[config.EsConn]{Conn: config.EsConn{URL: first}, Source: repoSrc(env)}
		}
	}
	return nil
}

// ─────────────────────────── s3 ───────────────────────────

// resolveS3 returns the configured S3 connection with the access key,
// secret key, region, and endpoint substituted from env when written
// as `$NAME` / `${NAME}` refs. Unlike the SQL engines, S3 has no
// widely-used env-derived DSN convention (`S3_URL` etc.) — the
// connection is YAML-only.
func resolveS3(cfg *config.Config, env envfile.EnvFile) *resolvedConn[config.S3Conn] {
	if cfg.Connections.S3 == nil {
		return nil
	}
	s := *cfg.Connections.S3
	s.AccessKey = resolvePasswordValue(env, s.AccessKey)
	s.SecretKey = resolvePasswordValue(env, s.SecretKey)
	s.Region = resolvePasswordValue(env, s.Region)
	s.Endpoint = resolvePasswordValue(env, s.Endpoint)
	return &resolvedConn[config.S3Conn]{Conn: s, Source: Source{Kind: SourceYaml}}
}

// ─────────────────────────── url helpers ───────────────────────────

// parseJDBC strips a leading `jdbc:` and returns the parsed url plus
// the engine name resolved from the scheme.
func parseJDBC(raw string) (*url.URL, string) {
	raw = strings.TrimPrefix(raw, "jdbc:")
	u, err := url.Parse(raw)
	if err != nil {
		return nil, ""
	}
	return u, normalizeEngineScheme(u.Scheme)
}

func normalizeEngineScheme(s string) string {
	switch s {
	case "mysql":
		return "mysql"
	case "mariadb":
		return "mariadb"
	case "tidb":
		return "tidb"
	case "postgres", "postgresql", "pg":
		return "postgres"
	case "cockroach", "cockroachdb":
		return "cockroach"
	case "redis", "rediss":
		return "redis"
	case "mongodb", "mongodb+srv":
		return "mongodb"
	}
	return ""
}

func pickURL(env envfile.EnvFile, primary string, engines []string) (*url.URL, Source, bool) {
	// Primary key in env-file.
	if v, ok := env.Get(primary); ok {
		if u, err := url.Parse(v); err == nil && containsString(engines, normalizeEngineScheme(u.Scheme)) {
			return u, repoSrc(env), true
		}
	}
	// DATABASE_URL in env-file.
	if v, ok := env.Get("DATABASE_URL"); ok {
		if u, err := url.Parse(v); err == nil && containsString(engines, normalizeEngineScheme(u.Scheme)) {
			return u, repoSrc(env), true
		}
	}
	return nil, Source{}, false
}

func mysqlFromURL(u *url.URL) config.MysqlConn {
	host := u.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	port := uint16(3306)
	if p := u.Port(); p != "" {
		if n, err := strconv.ParseUint(p, 10, 16); err == nil {
			port = uint16(n)
		}
	}
	user := "root"
	if u.User != nil {
		if n := u.User.Username(); n != "" {
			user = n
		}
	}
	return config.MysqlConn{Host: host, Port: port, User: user, PoolMax: 8}
}

func postgresFromURL(u *url.URL) config.PostgresConn {
	host := u.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	port := uint16(5432)
	if p := u.Port(); p != "" {
		if n, err := strconv.ParseUint(p, 10, 16); err == nil {
			port = uint16(n)
		}
	}
	user := "postgres"
	if u.User != nil {
		if n := u.User.Username(); n != "" {
			user = n
		}
	}
	return config.PostgresConn{Host: host, Port: port, User: user, PoolMax: 8}
}

func buildUserInfo(user, pass string) string {
	if nonEmpty(pass) && user != "" {
		return url.QueryEscape(user) + ":" + url.QueryEscape(pass) + "@"
	}
	if user != "" {
		return url.QueryEscape(user) + "@"
	}
	return ""
}

// ─────────────────────────── misc helpers ───────────────────────────

func nonEmpty(s string) bool {
	return s != "" && s != "null"
}

// urlPassword returns the URL's embedded password when present and
// non-empty. Centralises the `u.User != nil && Password() ok &&
// nonEmpty` dance repeated by every engine's URL/JDBC parsing.
func urlPassword(u *url.URL) (string, bool) {
	if u == nil || u.User == nil {
		return "", false
	}
	if p, ok := u.User.Password(); ok && nonEmpty(p) {
		return p, true
	}
	return "", false
}

// springPassword resolves the password for a Spring datasource: an
// explicit SPRING_DATASOURCE_PASSWORD wins, otherwise fall back to any
// password embedded in the JDBC URL.
func springPassword(env envfile.EnvFile, u *url.URL) (string, bool) {
	if pwd, ok := env.Get("SPRING_DATASOURCE_PASSWORD"); ok && nonEmpty(pwd) {
		return pwd, true
	}
	return urlPassword(u)
}

func firstEnv(env envfile.EnvFile, keys ...string) string {
	for _, k := range keys {
		if v, ok := env.Get(k); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstEnvOr(env envfile.EnvFile, fallback string, keys ...string) string {
	if v := firstEnv(env, keys...); v != "" {
		return v
	}
	return fallback
}

func envOr(env envfile.EnvFile, key, fallback string) string {
	if v, ok := env.Get(key); ok && v != "" {
		return v
	}
	return fallback
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func repoSrc(env envfile.EnvFile) Source {
	if env.Source != "" {
		return Source{Kind: SourceRepoEnvFile, Label: env.Source}
	}
	return Source{Kind: SourceDefault}
}
