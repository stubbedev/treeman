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
	"net/url"
	"path/filepath"
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
	env := loadRepoEnv(repoRoot, cfg.EnvSources)
	return Resolved{
		Mysql:         resolveMysql(cfg, env),
		Postgres:      resolvePostgres(cfg, env),
		Mongodb:       resolveMongodb(cfg, env),
		Redis:         resolveRedis(cfg, env),
		Elasticsearch: resolveElasticsearch(cfg, env),
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
func ApplyEnvCredentials(cfg *config.Config, repoRoot string) {
	r := Resolve(cfg, repoRoot)
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
	fillFromContainerEnv(cfg)
}

// fillFromContainerEnv populates empty passwords by inspecting the
// referenced container's env. Best-effort: any error is swallowed so
// a missing engine binary / unreachable daemon doesn't break config
// loading. Drivers will surface the real connectivity error later.
func fillFromContainerEnv(cfg *config.Config) {
	if m := cfg.Connections.Mysql; m != nil && m.Password == "" && (m.Container != "" || m.ComposeService != "") {
		env, _ := containerip.EnvLookup(containerip.Opts{
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
		env, _ := containerip.EnvLookup(containerip.Opts{
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
}

// loadRepoEnv reads exactly the env files declared in
// `env_scoping.sources` (last layer wins). When `sources` is empty
// the resolver reads nothing — no hidden default. `treeman init`
// emits a `sources:` list tailored to the detected framework so a
// fresh repo gets the conventional `.env*` lineup; modify the list
// to suit. Relative entries resolve against `repoRoot`; absolute
// entries are honoured as-is.
func loadRepoEnv(repoRoot string, sources []string) envfile.EnvFile {
	if repoRoot == "" || len(sources) == 0 {
		return envfile.EnvFile{Vars: map[string]string{}}
	}
	paths := make([]string, 0, len(sources))
	for _, s := range sources {
		if filepath.IsAbs(s) {
			paths = append(paths, s)
		} else {
			paths = append(paths, filepath.Join(repoRoot, s))
		}
	}
	return envfile.ReadLayered(paths)
}

// ─────────────────────────── mysql ───────────────────────────

func resolveMysql(cfg *config.Config, env envfile.EnvFile) *resolvedConn[config.MysqlConn] {
	if cfg.Connections.Mysql != nil {
		m := *cfg.Connections.Mysql
		m.Password = resolveMysqlPassword(env, m.PasswordEnv)
		return &resolvedConn[config.MysqlConn]{Conn: m, Source: Source{Kind: SourceYaml}}
	}
	// Spring Boot — SPRING_DATASOURCE_URL is JDBC-style; strip the
	// jdbc: prefix and parse.
	if v, ok := env.Get("SPRING_DATASOURCE_URL"); ok {
		if u, eng := parseJDBC(v); eng == "mysql" || eng == "mariadb" || eng == "tidb" {
			m := mysqlFromURL(u)
			if pwd, ok := env.Get("SPRING_DATASOURCE_PASSWORD"); ok && nonEmpty(pwd) {
				m.Password = pwd
			} else if u.User != nil {
				if p, ok := u.User.Password(); ok && nonEmpty(p) {
					m.Password = p
				}
			}
			if user, ok := env.Get("SPRING_DATASOURCE_USERNAME"); ok {
				m.User = user
			}
			return &resolvedConn[config.MysqlConn]{Conn: m, Source: repoSrc(env)}
		}
	}
	if u, src, ok := pickURL(env, "MYSQL_URL", []string{"mysql", "mariadb", "tidb"}); ok {
		m := mysqlFromURL(u)
		if u.User != nil {
			if p, ok := u.User.Password(); ok && nonEmpty(p) {
				m.Password = p
			}
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
			Password: resolveMysqlPassword(env, nil),
			PoolMax:  8,
		},
		Source: repoSrc(env),
	}
}

func resolveMysqlPassword(env envfile.EnvFile, passwordEnv *string) string {
	if passwordEnv != nil {
		if v, ok := env.Get(*passwordEnv); ok && nonEmpty(v) {
			return v
		}
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
		p.Password = resolvePostgresPassword(env, p.PasswordEnv)
		return &resolvedConn[config.PostgresConn]{Conn: p, Source: Source{Kind: SourceYaml}}
	}
	if v, ok := env.Get("SPRING_DATASOURCE_URL"); ok {
		if u, eng := parseJDBC(v); eng == "postgres" || eng == "cockroach" {
			p := postgresFromURL(u)
			if pwd, ok := env.Get("SPRING_DATASOURCE_PASSWORD"); ok && nonEmpty(pwd) {
				p.Password = pwd
			} else if u.User != nil {
				if pp, ok := u.User.Password(); ok && nonEmpty(pp) {
					p.Password = pp
				}
			}
			if user, ok := env.Get("SPRING_DATASOURCE_USERNAME"); ok {
				p.User = user
			}
			return &resolvedConn[config.PostgresConn]{Conn: p, Source: repoSrc(env)}
		}
	}
	if u, src, ok := pickURL(env, "POSTGRES_URL", []string{"postgres", "postgresql", "cockroach", "cockroachdb"}); ok {
		p := postgresFromURL(u)
		if u.User != nil {
			if pp, ok := u.User.Password(); ok && nonEmpty(pp) {
				p.Password = pp
			}
		}
		return &resolvedConn[config.PostgresConn]{Conn: p, Source: src}
	}
	if u, src, ok := pickURL(env, "PG_URL", []string{"postgres", "postgresql", "cockroach", "cockroachdb"}); ok {
		p := postgresFromURL(u)
		if u.User != nil {
			if pp, ok := u.User.Password(); ok && nonEmpty(pp) {
				p.Password = pp
			}
		}
		return &resolvedConn[config.PostgresConn]{Conn: p, Source: src}
	}
	return nil
}

func resolvePostgresPassword(env envfile.EnvFile, passwordEnv *string) string {
	if passwordEnv != nil {
		if v, ok := env.Get(*passwordEnv); ok && nonEmpty(v) {
			return v
		}
	}
	for _, k := range []string{"DB_TEST_PASSWORD", "DB_PASSWORD", "PGPASSWORD", "POSTGRES_PASSWORD"} {
		if v, ok := env.Get(k); ok && nonEmpty(v) {
			return v
		}
	}
	return ""
}

// ─────────────────────────── mongo ───────────────────────────

func resolveMongodb(cfg *config.Config, env envfile.EnvFile) *resolvedConn[config.MongoConn] {
	if cfg.Connections.Mongodb != nil {
		return &resolvedConn[config.MongoConn]{Conn: *cfg.Connections.Mongodb, Source: Source{Kind: SourceYaml}}
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
		return &resolvedConn[config.RedisConn]{Conn: *cfg.Connections.Redis, Source: Source{Kind: SourceYaml}}
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
		return &resolvedConn[config.EsConn]{Conn: *cfg.Connections.Elasticsearch, Source: Source{Kind: SourceYaml}}
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
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func repoSrc(env envfile.EnvFile) Source {
	if env.Source != "" {
		return Source{Kind: SourceRepoEnvFile, Label: env.Source}
	}
	return Source{Kind: SourceDefault}
}
