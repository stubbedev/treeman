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
    if let Some(m) = cfg.connections.mysql.clone() {
        return Some((m, Source::Yaml));
    }
    if let Some((u, src)) = pick_url(
        env,
        "MYSQL_URL",
        &[Engine::Mysql, Engine::Mariadb, Engine::Tidb],
    ) {
        return Some((mysql_from_url(&u), src));
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
        return Some((
            MysqlConn {
                host: host.to_string(),
                port,
                user,
                password_env: None,
                pool_max: 8,
            },
            source,
        ));
    }
    None
}

fn resolve_postgres(cfg: &Config, env: &EnvFile) -> Option<(PostgresConn, Source)> {
    if cfg.connections.postgres.is_some() {
        return Some((cfg.connections.postgres.clone().unwrap(), Source::Yaml));
    }
    if let Some((u, src)) = pick_url(env, "POSTGRES_URL", &[Engine::Postgres, Engine::Cockroach]) {
        return Some((postgres_from_url(&u), src));
    }
    if let Some((u, src)) = pick_url(env, "PG_URL", &[Engine::Postgres, Engine::Cockroach]) {
        return Some((postgres_from_url(&u), src));
    }
    None
}

fn resolve_mongodb(cfg: &Config, env: &EnvFile) -> Option<(MongoConn, Source)> {
    if cfg.connections.mongodb.is_some() {
        return Some((cfg.connections.mongodb.clone().unwrap(), Source::Yaml));
    }
    if let Some(v) = env.get("MONGODB_URI").or_else(|| env.get("MONGO_URL")) {
        let src = env
            .source
            .clone()
            .map(Source::RepoEnvFile)
            .unwrap_or(Source::Default);
        return Some((MongoConn { uri: v.to_string() }, src));
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
    if let Ok(v) = std::env::var("REDIS_URL") {
        return Some((RedisConn { url: v }, Source::EnvUrl("REDIS_URL".into())));
    }
    None
}

fn resolve_elasticsearch(cfg: &Config, env: &EnvFile) -> Option<(EsConn, Source)> {
    if cfg.connections.elasticsearch.is_some() {
        return Some((cfg.connections.elasticsearch.clone().unwrap(), Source::Yaml));
    }
    if let Some(v) = env
        .get("ELASTICSEARCH_URL")
        .or_else(|| env.get("ELASTIC_URL"))
        .or_else(|| env.get("OPENSEARCH_URL"))
    {
        return Some((
            EsConn { url: v.to_string() },
            env.source
                .clone()
                .map(Source::RepoEnvFile)
                .unwrap_or(Source::Default),
        ));
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
        pool_max: 8,
    }
}

fn postgres_from_url(u: &DbUrl) -> PostgresConn {
    PostgresConn {
        host: u.host.clone().unwrap_or_else(|| "127.0.0.1".into()),
        port: u.port.unwrap_or(5432),
        user: u.user.clone().unwrap_or_else(|| "postgres".into()),
        password_env: None,
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
}
