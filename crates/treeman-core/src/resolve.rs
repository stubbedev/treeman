//! Connection-parameter resolver. Layered, transparent precedence:
//!
//!   1. Explicit `connections.*` in `.treeman.yaml` (with
//!      `password_env` looked up in the process env).
//!   2. Per-engine URL env vars (`MYSQL_URL`, `POSTGRES_URL`/`PG_URL`,
//!      `MONGO_URL`/`MONGODB_URI`, `REDIS_URL`, `ELASTICSEARCH_URL`/…).
//!   3. Generic `DATABASE_URL` (used by sqlx-cli, Diesel, Symfony,
//!      Prisma when the scheme matches the engine).
//!   4. Repo env files — `.env`, `.env.testing`, `.env.test`,
//!      `.env.local`, `.env.testing.local` (Laravel + Symfony +
//!      dotenv-style projects). Reads `DB_HOST`/`DB_PORT`/`DB_USERNAME`
//!      /`DB_PASSWORD`/`DB_DATABASE` plus the `DB_TEST_*` overrides
//!      used by Laravel.
//!
//! The result is a `ResolvedConnections` carrying both the merged
//! `connections::*` values and a per-engine provenance trail for
//! `treeman config show --resolved`.

use std::path::Path;

use crate::config::{Config, EsConn, MongoConn, MysqlConn, PostgresConn, RedisConn};
use crate::dburl::{self, DbUrl, Engine};
use crate::envfile::{EnvFile, read_layered};

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Source {
    Yaml,
    EnvUrl(String),
    DatabaseUrl,
    RepoEnvFile(std::path::PathBuf),
    Default,
}

#[derive(Debug, Clone, Default)]
pub struct ResolvedConnections {
    pub mysql: Option<(MysqlConn, Source)>,
    pub postgres: Option<(PostgresConn, Source)>,
    pub mongodb: Option<(MongoConn, Source)>,
    pub redis: Option<(RedisConn, Source)>,
    pub elasticsearch: Option<(EsConn, Source)>,
}

/// Resolve every supported connection given the parsed config and
/// (optionally) a repo root to scan for `.env*` files.
pub fn resolve(cfg: &Config, repo_root: Option<&Path>) -> ResolvedConnections {
    let env = load_repo_env(repo_root);
    ResolvedConnections {
        mysql: resolve_mysql(cfg, &env),
        postgres: resolve_postgres(cfg, &env),
        mongodb: resolve_mongodb(cfg, &env),
        redis: resolve_redis(cfg, &env),
        elasticsearch: resolve_elasticsearch(cfg, &env),
    }
}

fn load_repo_env(repo_root: Option<&Path>) -> EnvFile {
    let Some(root) = repo_root else {
        return EnvFile::default();
    };
    // Laravel/Symfony/dotenv ordering. Later files override earlier.
    let paths: Vec<std::path::PathBuf> = [
        ".env",
        ".env.local",
        ".env.test",
        ".env.testing",
        ".env.test.local",
        ".env.testing.local",
    ]
    .iter()
    .map(|n| root.join(n))
    .collect();
    read_layered(&paths)
}

// ───────────────────────── per-engine resolvers ─────────────────────────

fn resolve_mysql(cfg: &Config, env: &EnvFile) -> Option<(MysqlConn, Source)> {
    if let Some(mut m) = cfg.connections.mysql.clone() {
        // YAML provides host/port/user. Pull password from .env (and
        // process env as fallback) so users don't have to bake creds
        // into a systemd-user override.
        m.password = resolve_mysql_password(env, m.password_env.as_deref());
        return Some((m, Source::Yaml));
    }
    // Spring Boot — SPRING_DATASOURCE_URL is JDBC-style. dburl::parse
    // strips the `jdbc:` prefix transparently.
    if let Some(v) = env
        .get("SPRING_DATASOURCE_URL")
        .or_else(|| env.get("SPRING_R2DBC_URL"))
    {
        if let Ok(u) = dburl::parse(v) {
            if matches!(u.engine, Engine::Mysql | Engine::Mariadb | Engine::Tidb) {
                let mut mc = mysql_from_url(&u);
                if let Some(p) = env
                    .get("SPRING_DATASOURCE_PASSWORD")
                    .or(u.password.as_deref())
                    .filter(|s| !s.is_empty() && *s != "null")
                {
                    mc.password = Some(p.to_string());
                }
                if let Some(user) = env.get("SPRING_DATASOURCE_USERNAME") {
                    mc.user = user.to_string();
                }
                let src = env
                    .source
                    .clone()
                    .map(Source::RepoEnvFile)
                    .unwrap_or(Source::Default);
                return Some((mc, src));
            }
        }
    }
    if let Some((u, src)) = pick_url(
        env,
        "MYSQL_URL",
        &[Engine::Mysql, Engine::Mariadb, Engine::Tidb],
    ) {
        let mut mc = mysql_from_url(&u);
        if let Some(p) = u.password.as_deref().filter(|s| !s.is_empty()) {
            mc.password = Some(p.to_string());
        }
        return Some((mc, src));
    }
    // Laravel `DB_*` (preferring `DB_TEST_*` overrides for testing).
    if let Some(host) = env.get("DB_TEST_HOST").or_else(|| env.get("DB_HOST")) {
        let port = env
            .get("DB_TEST_PORT")
            .or_else(|| env.get("DB_PORT"))
            .and_then(|p| p.parse().ok())
            .unwrap_or(3306);
        let user = env
            .get("DB_TEST_USERNAME")
            .or_else(|| env.get("DB_USERNAME"))
            .unwrap_or("root")
            .to_string();
        let source = env
            .source
            .clone()
            .map(Source::RepoEnvFile)
            .unwrap_or(Source::Default);
        let password = resolve_mysql_password(env, None);
        return Some((
            MysqlConn {
                host: host.to_string(),
                port,
                user,
                password_env: None,
                password,
                pool_max: 8,
            },
            source,
        ));
    }
    None
}

/// Pull a mysql password out of `env` and the process env, preferring
/// the YAML-declared `password_env` key if any. Returns None when no
/// non-empty password is found.
fn resolve_mysql_password(env: &EnvFile, password_env: Option<&str>) -> Option<String> {
    // 1. Explicit `password_env` key in YAML.
    if let Some(key) = password_env {
        if let Some(v) = env.get(key) {
            if !v.is_empty() && v != "null" {
                return Some(v.to_string());
            }
        }
        if let Ok(v) = std::env::var(key) {
            if !v.is_empty() && v != "null" {
                return Some(v);
            }
        }
    }
    // 2. Conventional Laravel `DB_TEST_PASSWORD` / `DB_PASSWORD`.
    for k in [
        "DB_TEST_PASSWORD",
        "DB_PASSWORD",
        "MYSQL_PASSWORD",
        "MYSQL_PWD",
    ] {
        if let Some(v) = env.get(k) {
            if !v.is_empty() && v != "null" {
                return Some(v.to_string());
            }
        }
        if let Ok(v) = std::env::var(k) {
            if !v.is_empty() && v != "null" {
                return Some(v);
            }
        }
    }
    None
}

fn resolve_postgres_password(env: &EnvFile, password_env: Option<&str>) -> Option<String> {
    if let Some(key) = password_env {
        if let Some(v) = env.get(key) {
            if !v.is_empty() && v != "null" {
                return Some(v.to_string());
            }
        }
        if let Ok(v) = std::env::var(key) {
            if !v.is_empty() && v != "null" {
                return Some(v);
            }
        }
    }
    for k in [
        "DB_TEST_PASSWORD",
        "DB_PASSWORD",
        "PGPASSWORD",
        "POSTGRES_PASSWORD",
    ] {
        if let Some(v) = env.get(k) {
            if !v.is_empty() && v != "null" {
                return Some(v.to_string());
            }
        }
        if let Ok(v) = std::env::var(k) {
            if !v.is_empty() && v != "null" {
                return Some(v);
            }
        }
    }
    None
}

fn resolve_postgres(cfg: &Config, env: &EnvFile) -> Option<(PostgresConn, Source)> {
    if let Some(mut p) = cfg.connections.postgres.clone() {
        p.password = resolve_postgres_password(env, p.password_env.as_deref());
        return Some((p, Source::Yaml));
    }
    // Spring Boot — JDBC-style postgres URL.
    if let Some(v) = env
        .get("SPRING_DATASOURCE_URL")
        .or_else(|| env.get("SPRING_R2DBC_URL"))
    {
        if let Ok(u) = dburl::parse(v) {
            if matches!(u.engine, Engine::Postgres | Engine::Cockroach) {
                let mut pc = postgres_from_url(&u);
                if let Some(pw) = env
                    .get("SPRING_DATASOURCE_PASSWORD")
                    .or(u.password.as_deref())
                    .filter(|s| !s.is_empty() && *s != "null")
                {
                    pc.password = Some(pw.to_string());
                }
                if let Some(user) = env.get("SPRING_DATASOURCE_USERNAME") {
                    pc.user = user.to_string();
                }
                let src = env
                    .source
                    .clone()
                    .map(Source::RepoEnvFile)
                    .unwrap_or(Source::Default);
                return Some((pc, src));
            }
        }
    }
    if let Some((u, src)) = pick_url(env, "POSTGRES_URL", &[Engine::Postgres, Engine::Cockroach]) {
        let mut pc = postgres_from_url(&u);
        if let Some(p) = u.password.as_deref().filter(|s| !s.is_empty()) {
            pc.password = Some(p.to_string());
        }
        return Some((pc, src));
    }
    if let Some((u, src)) = pick_url(env, "PG_URL", &[Engine::Postgres, Engine::Cockroach]) {
        let mut pc = postgres_from_url(&u);
        if let Some(p) = u.password.as_deref().filter(|s| !s.is_empty()) {
            pc.password = Some(p.to_string());
        }
        return Some((pc, src));
    }
    None
}

fn resolve_mongodb(cfg: &Config, env: &EnvFile) -> Option<(MongoConn, Source)> {
    if cfg.connections.mongodb.is_some() {
        return Some((cfg.connections.mongodb.clone().unwrap(), Source::Yaml));
    }
    // Accept all of: MONGODB_URI (Node, official driver default),
    // MONGO_URL (Symfony, Heroku), MONGO_URI (Django/PyMongo),
    // MONGODB_URL (Symfony alias), SPRING_DATA_MONGODB_URI (Spring Boot).
    for k in [
        "MONGODB_URI",
        "MONGO_URL",
        "MONGO_URI",
        "MONGODB_URL",
        "SPRING_DATA_MONGODB_URI",
    ] {
        if let Some(v) = env.get(k) {
            let src = env
                .source
                .clone()
                .map(Source::RepoEnvFile)
                .unwrap_or(Source::Default);
            return Some((MongoConn { uri: v.to_string() }, src));
        }
    }
    // Spring Boot — compose from componentized properties.
    if let Some(host) = env.get("SPRING_DATA_MONGODB_HOST") {
        let port = env.get("SPRING_DATA_MONGODB_PORT").unwrap_or("27017");
        let user = env.get("SPRING_DATA_MONGODB_USERNAME");
        let pass = env.get("SPRING_DATA_MONGODB_PASSWORD");
        let userinfo = match (user, pass) {
            (Some(u), Some(p)) if !p.is_empty() && p != "null" => {
                format!("{}:{}@", percent_encode(u), percent_encode(p))
            }
            (Some(u), _) => format!("{}@", percent_encode(u)),
            _ => String::new(),
        };
        let db = env
            .get("SPRING_DATA_MONGODB_DATABASE")
            .map(|d| format!("/{d}"))
            .unwrap_or_default();
        let src = env
            .source
            .clone()
            .map(Source::RepoEnvFile)
            .unwrap_or(Source::Default);
        return Some((
            MongoConn {
                uri: format!("mongodb://{userinfo}{host}:{port}{db}"),
            },
            src,
        ));
    }
    // Compose from `MONGO_DB_HOST` / `MONGO_DB_PORT` (Laravel convention)
    // or generic `MONGO_HOST` / `MONGO_PORT`.
    if let Some(host) = env.get("MONGO_DB_HOST").or_else(|| env.get("MONGO_HOST")) {
        let port = env
            .get("MONGO_DB_PORT")
            .or_else(|| env.get("MONGO_PORT"))
            .unwrap_or("27017");
        let user = env
            .get("MONGO_DB_USERNAME")
            .or_else(|| env.get("MONGO_USERNAME"));
        let pass = env
            .get("MONGO_DB_PASSWORD")
            .or_else(|| env.get("MONGO_PASSWORD"));
        let userinfo = match (user, pass) {
            (Some(u), Some(p)) if !p.is_empty() && p != "null" => {
                format!("{}:{}@", percent_encode(u), percent_encode(p))
            }
            (Some(u), _) => format!("{}@", percent_encode(u)),
            _ => String::new(),
        };
        let uri = format!("mongodb://{userinfo}{host}:{port}");
        let src = env
            .source
            .clone()
            .map(Source::RepoEnvFile)
            .unwrap_or(Source::Default);
        return Some((MongoConn { uri }, src));
    }
    if let Some(v) = std::env::var("MONGODB_URI")
        .ok()
        .or_else(|| std::env::var("MONGO_URL").ok())
    {
        return Some((MongoConn { uri: v }, Source::EnvUrl("MONGODB_URI".into())));
    }
    None
}

fn resolve_redis(cfg: &Config, env: &EnvFile) -> Option<(RedisConn, Source)> {
    if cfg.connections.redis.is_some() {
        return Some((cfg.connections.redis.clone().unwrap(), Source::Yaml));
    }
    if let Some(v) = env.get("REDIS_URL") {
        return Some((
            RedisConn { url: v.to_string() },
            env.source
                .clone()
                .map(Source::RepoEnvFile)
                .unwrap_or(Source::Default),
        ));
    }
    // Spring Boot (3.x: `spring.data.redis.*`; 2.x legacy:
    // `spring.redis.*`). Fall through to the generic REDIS_HOST path
    // for Laravel / Node / Django.
    let spring_host = env
        .get("SPRING_DATA_REDIS_HOST")
        .or_else(|| env.get("SPRING_REDIS_HOST"));
    if let Some(host) = spring_host {
        let port = env
            .get("SPRING_DATA_REDIS_PORT")
            .or_else(|| env.get("SPRING_REDIS_PORT"))
            .unwrap_or("6379");
        let pass = env
            .get("SPRING_DATA_REDIS_PASSWORD")
            .or_else(|| env.get("SPRING_REDIS_PASSWORD"));
        let userinfo = match pass {
            Some(p) if !p.is_empty() && p != "null" => {
                format!(":{}@", percent_encode(p))
            }
            _ => String::new(),
        };
        let url = format!("redis://{userinfo}{host}:{port}");
        let src = env
            .source
            .clone()
            .map(Source::RepoEnvFile)
            .unwrap_or(Source::Default);
        return Some((RedisConn { url }, src));
    }
    // Compose from `REDIS_HOST` / `REDIS_PORT` / `REDIS_PASSWORD`. The
    // Laravel convention writes the literal `null` when no password is
    // set — skip those so we don't paste it into the URL.
    if let Some(host) = env.get("REDIS_HOST") {
        let port = env.get("REDIS_PORT").unwrap_or("6379");
        let pass = env.get("REDIS_PASSWORD");
        let userinfo = match pass {
            Some(p) if !p.is_empty() && p != "null" => {
                format!(":{}@", percent_encode(p))
            }
            _ => String::new(),
        };
        let url = format!("redis://{userinfo}{host}:{port}");
        let src = env
            .source
            .clone()
            .map(Source::RepoEnvFile)
            .unwrap_or(Source::Default);
        return Some((RedisConn { url }, src));
    }
    if let Ok(v) = std::env::var("REDIS_URL") {
        return Some((RedisConn { url: v }, Source::EnvUrl("REDIS_URL".into())));
    }
    None
}

fn resolve_elasticsearch(cfg: &Config, env: &EnvFile) -> Option<(EsConn, Source)> {
    if cfg.connections.elasticsearch.is_some() {
        return Some((cfg.connections.elasticsearch.clone().unwrap(), Source::Yaml));
    }
    // Accept all of: ELASTICSEARCH_URL (Node), ELASTIC_URL (variant),
    // OPENSEARCH_URL (AWS / OpenSearch fork), SPRING_ELASTICSEARCH_URIS
    // (Spring Boot — may be comma-separated).
    for k in [
        "ELASTICSEARCH_URL",
        "ELASTIC_URL",
        "OPENSEARCH_URL",
        "SPRING_ELASTICSEARCH_URIS",
    ] {
        if let Some(v) = env.get(k) {
            // Spring's *_URIS is a comma-separated list — take the first.
            let first = v.split(',').next().unwrap_or(v).trim();
            if !first.is_empty() {
                let url = if first.contains("://") {
                    first.to_string()
                } else {
                    format!("http://{first}")
                };
                let src = env
                    .source
                    .clone()
                    .map(Source::RepoEnvFile)
                    .unwrap_or(Source::Default);
                return Some((EsConn { url }, src));
            }
        }
    }
    // Laravel's `ELASTICSEARCH_HOSTS` may be `host:port` (no scheme) or
    // a comma-separated list. Same for ELASTIC_HOSTS / ES_HOSTS /
    // ELASTICSEARCH_DSL_HOSTS (Django).
    let hosts_raw = [
        "ELASTICSEARCH_HOSTS",
        "ELASTIC_HOSTS",
        "ES_HOSTS",
        "ELASTICSEARCH_DSL_HOSTS",
    ]
    .iter()
    .find_map(|k| env.get(k));
    if let Some(raw) = hosts_raw {
        let first = raw.split(',').next().unwrap_or(raw).trim();
        if !first.is_empty() {
            let url = if first.contains("://") {
                first.to_string()
            } else {
                format!("http://{first}")
            };
            let src = env
                .source
                .clone()
                .map(Source::RepoEnvFile)
                .unwrap_or(Source::Default);
            return Some((EsConn { url }, src));
        }
    }
    if let Ok(v) = std::env::var("ELASTICSEARCH_URL").or_else(|_| std::env::var("OPENSEARCH_URL")) {
        return Some((
            EsConn { url: v },
            Source::EnvUrl("ELASTICSEARCH_URL".into()),
        ));
    }
    None
}

// ───────────────────────── URL helpers ─────────────────────────

/// Search env file + process env for an engine URL, ordered by preference.
/// Returns the first matching parsed DbUrl whose engine is in `match_engines`.
fn pick_url(env: &EnvFile, primary: &str, match_engines: &[Engine]) -> Option<(DbUrl, Source)> {
    // 1. env-file primary key (e.g. MYSQL_URL inside .env.testing)
    if let Some(v) = env.get(primary) {
        if let Ok(u) = dburl::parse(v) {
            if match_engines.contains(&u.engine) {
                return Some((
                    u,
                    env.source
                        .clone()
                        .map(Source::RepoEnvFile)
                        .unwrap_or(Source::Default),
                ));
            }
        }
    }
    // 2. env-file DATABASE_URL when the scheme matches
    if let Some(v) = env.get("DATABASE_URL") {
        if let Ok(u) = dburl::parse(v) {
            if match_engines.contains(&u.engine) {
                return Some((
                    u,
                    env.source
                        .clone()
                        .map(Source::RepoEnvFile)
                        .unwrap_or(Source::Default),
                ));
            }
        }
    }
    // 3. process env primary
    if let Ok(v) = std::env::var(primary) {
        if let Ok(u) = dburl::parse(&v) {
            if match_engines.contains(&u.engine) {
                return Some((u, Source::EnvUrl(primary.into())));
            }
        }
    }
    // 4. process env DATABASE_URL
    if let Ok(v) = std::env::var("DATABASE_URL") {
        if let Ok(u) = dburl::parse(&v) {
            if match_engines.contains(&u.engine) {
                return Some((u, Source::DatabaseUrl));
            }
        }
    }
    None
}

fn mysql_from_url(u: &DbUrl) -> MysqlConn {
    MysqlConn {
        host: u.host.clone().unwrap_or_else(|| "127.0.0.1".into()),
        port: u.port.unwrap_or(3306),
        user: u.user.clone().unwrap_or_else(|| "root".into()),
        password_env: None,
        password: None,
        pool_max: 8,
    }
}

/// Minimal RFC-3986 percent-encoding for userinfo segments. We only
/// need to escape characters that would break URI parsing (`:`, `@`,
/// `/`, `?`, `#`, space). Anything ASCII alphanumeric or in the
/// unreserved set passes through.
fn percent_encode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '~') {
            out.push(c);
        } else {
            let mut buf = [0u8; 4];
            for b in c.encode_utf8(&mut buf).bytes() {
                out.push_str(&format!("%{:02X}", b));
            }
        }
    }
    out
}

fn postgres_from_url(u: &DbUrl) -> PostgresConn {
    PostgresConn {
        host: u.host.clone().unwrap_or_else(|| "127.0.0.1".into()),
        port: u.port.unwrap_or(5432),
        user: u.user.clone().unwrap_or_else(|| "postgres".into()),
        password_env: None,
        password: None,
        pool_max: 8,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::envfile::parse;

    #[test]
    fn yaml_wins() {
        let cfg = Config::default();
        let mut cfg = cfg;
        cfg.connections.mysql = Some(MysqlConn {
            host: "h".into(),
            port: 3306,
            user: "u".into(),
            password_env: None,
            password: None,
            pool_max: 8,
        });
        let r = resolve(&cfg, None);
        assert!(matches!(r.mysql, Some((_, Source::Yaml))));
    }

    #[test]
    fn laravel_db_test_overrides_win() {
        let cfg = Config::default();
        let env_txt = "DB_HOST=base\nDB_TEST_HOST=test-host\nDB_TEST_PORT=3307\n";
        let env = parse(env_txt);
        let m = resolve_mysql(&cfg, &env).map(|(c, _)| c).unwrap();
        assert_eq!(m.host, "test-host");
        assert_eq!(m.port, 3307);
    }

    #[test]
    fn database_url_matches_engine() {
        let env = parse("DATABASE_URL=postgres://x@h:5432/d\n");
        let cfg = Config::default();
        assert!(resolve_postgres(&cfg, &env).is_some());
        assert!(resolve_mysql(&cfg, &env).is_none());
    }

    #[test]
    fn yaml_mysql_picks_up_password_from_env_file() {
        let mut cfg = Config::default();
        cfg.connections.mysql = Some(MysqlConn {
            host: "127.0.0.1".into(),
            port: 3306,
            user: "myapp".into(),
            password_env: Some("DB_TEST_PASSWORD".into()),
            password: None,
            pool_max: 8,
        });
        let env = parse("DB_TEST_PASSWORD=secret-pw\n");
        let (m, src) = resolve_mysql(&cfg, &env).unwrap();
        assert_eq!(m.password.as_deref(), Some("secret-pw"));
        assert!(matches!(src, Source::Yaml));
    }

    #[test]
    fn yaml_mysql_password_falls_back_to_db_password_when_no_env_key_declared() {
        let mut cfg = Config::default();
        cfg.connections.mysql = Some(MysqlConn {
            host: "127.0.0.1".into(),
            port: 3306,
            user: "root".into(),
            password_env: None,
            password: None,
            pool_max: 8,
        });
        let env = parse("DB_PASSWORD=fallback\n");
        let (m, _) = resolve_mysql(&cfg, &env).unwrap();
        assert_eq!(m.password.as_deref(), Some("fallback"));
    }

    #[test]
    fn redis_url_composed_from_componentized_env() {
        let cfg = Config::default();
        let env = parse("REDIS_HOST=127.0.0.1\nREDIS_PORT=6390\nREDIS_PASSWORD=hello/world\n");
        let (r, _) = resolve_redis(&cfg, &env).unwrap();
        assert_eq!(r.url, "redis://:hello%2Fworld@127.0.0.1:6390");
    }

    #[test]
    fn redis_skips_literal_null_password() {
        let cfg = Config::default();
        let env = parse("REDIS_HOST=127.0.0.1\nREDIS_PORT=6379\nREDIS_PASSWORD=null\n");
        let (r, _) = resolve_redis(&cfg, &env).unwrap();
        assert_eq!(r.url, "redis://127.0.0.1:6379");
    }

    #[test]
    fn elasticsearch_hosts_gains_http_scheme() {
        let cfg = Config::default();
        let env = parse("ELASTICSEARCH_HOSTS=localhost:9200\n");
        let (e, _) = resolve_elasticsearch(&cfg, &env).unwrap();
        assert_eq!(e.url, "http://localhost:9200");
    }

    #[test]
    fn mongo_uri_composed_from_host_port() {
        let cfg = Config::default();
        let env = parse("MONGO_DB_HOST=localhost\nMONGO_DB_PORT=27017\n");
        let (m, _) = resolve_mongodb(&cfg, &env).unwrap();
        assert_eq!(m.uri, "mongodb://localhost:27017");
    }

    #[test]
    fn spring_boot_jdbc_mysql_url() {
        let cfg = Config::default();
        let env = parse(
            "SPRING_DATASOURCE_URL=jdbc:mysql://db.internal:3307/app\n\
             SPRING_DATASOURCE_USERNAME=spring\n\
             SPRING_DATASOURCE_PASSWORD=letmein\n",
        );
        let (m, _) = resolve_mysql(&cfg, &env).unwrap();
        assert_eq!(m.host, "db.internal");
        assert_eq!(m.port, 3307);
        assert_eq!(m.user, "spring");
        assert_eq!(m.password.as_deref(), Some("letmein"));
    }

    #[test]
    fn spring_boot_jdbc_postgres_url() {
        let cfg = Config::default();
        let env = parse(
            "SPRING_DATASOURCE_URL=jdbc:postgresql://pg.internal/app\n\
             SPRING_DATASOURCE_USERNAME=pg\n\
             SPRING_DATASOURCE_PASSWORD=pgsecret\n",
        );
        let (p, _) = resolve_postgres(&cfg, &env).unwrap();
        assert_eq!(p.host, "pg.internal");
        assert_eq!(p.user, "pg");
        assert_eq!(p.password.as_deref(), Some("pgsecret"));
    }

    #[test]
    fn spring_boot_mongo_uri() {
        let cfg = Config::default();
        let env = parse("SPRING_DATA_MONGODB_URI=mongodb://mh:27018/app\n");
        let (m, _) = resolve_mongodb(&cfg, &env).unwrap();
        assert_eq!(m.uri, "mongodb://mh:27018/app");
    }

    #[test]
    fn spring_boot_mongo_composed() {
        let cfg = Config::default();
        let env = parse(
            "SPRING_DATA_MONGODB_HOST=mh\n\
             SPRING_DATA_MONGODB_PORT=27020\n\
             SPRING_DATA_MONGODB_USERNAME=u\n\
             SPRING_DATA_MONGODB_PASSWORD=p\n\
             SPRING_DATA_MONGODB_DATABASE=appdb\n",
        );
        let (m, _) = resolve_mongodb(&cfg, &env).unwrap();
        assert_eq!(m.uri, "mongodb://u:p@mh:27020/appdb");
    }

    #[test]
    fn spring_boot_redis() {
        let cfg = Config::default();
        let env = parse(
            "SPRING_DATA_REDIS_HOST=rh\n\
             SPRING_DATA_REDIS_PORT=6390\n\
             SPRING_DATA_REDIS_PASSWORD=rpw\n",
        );
        let (r, _) = resolve_redis(&cfg, &env).unwrap();
        assert_eq!(r.url, "redis://:rpw@rh:6390");
    }

    #[test]
    fn spring_legacy_redis_keys() {
        let cfg = Config::default();
        let env = parse("SPRING_REDIS_HOST=rh\nSPRING_REDIS_PORT=6379\n");
        let (r, _) = resolve_redis(&cfg, &env).unwrap();
        assert_eq!(r.url, "redis://rh:6379");
    }

    #[test]
    fn symfony_mongodb_url_alias() {
        let cfg = Config::default();
        let env = parse("MONGODB_URL=mongodb://localhost:27017/sf\n");
        let (m, _) = resolve_mongodb(&cfg, &env).unwrap();
        assert_eq!(m.uri, "mongodb://localhost:27017/sf");
    }

    #[test]
    fn django_pymongo_uri() {
        let cfg = Config::default();
        let env = parse("MONGO_URI=mongodb://localhost:27017/django\n");
        let (m, _) = resolve_mongodb(&cfg, &env).unwrap();
        assert_eq!(m.uri, "mongodb://localhost:27017/django");
    }

    #[test]
    fn django_dsl_elasticsearch_hosts() {
        let cfg = Config::default();
        let env = parse("ELASTICSEARCH_DSL_HOSTS=es.local:9200\n");
        let (e, _) = resolve_elasticsearch(&cfg, &env).unwrap();
        assert_eq!(e.url, "http://es.local:9200");
    }

    #[test]
    fn spring_es_uris_comma_separated() {
        let cfg = Config::default();
        let env = parse("SPRING_ELASTICSEARCH_URIS=https://es-1:9200,https://es-2:9200\n");
        let (e, _) = resolve_elasticsearch(&cfg, &env).unwrap();
        // First entry wins.
        assert_eq!(e.url, "https://es-1:9200");
    }

    #[test]
    fn null_password_in_env_yields_no_password() {
        let mut cfg = Config::default();
        cfg.connections.mysql = Some(MysqlConn {
            host: "h".into(),
            port: 3306,
            user: "u".into(),
            password_env: Some("DB_PASSWORD".into()),
            password: None,
            pool_max: 8,
        });
        let env = parse("DB_PASSWORD=null\n");
        let (m, _) = resolve_mysql(&cfg, &env).unwrap();
        assert!(m.password.is_none());
    }
}
