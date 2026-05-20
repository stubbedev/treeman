//! Pure-wire database drivers. No shelling out to mysql/psql/mongosh/etc.
//!
//! M4 ships mysql + redis. Postgres, MongoDB, Elasticsearch land in
//! later milestones.

pub mod mysql;
pub mod postgres;
pub mod redis_driver;

use async_trait::async_trait;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Engine {
    Mysql,
    Postgres,
    Mongodb,
    Redis,
    Elasticsearch,
    Sqlite,
}

impl Engine {
    pub fn as_str(self) -> &'static str {
        match self {
            Engine::Mysql => "mysql",
            Engine::Postgres => "postgres",
            Engine::Mongodb => "mongodb",
            Engine::Redis => "redis",
            Engine::Elasticsearch => "elasticsearch",
            Engine::Sqlite => "sqlite",
        }
    }
}

#[derive(Debug, Clone)]
pub enum Namespace {
    /// Top-level database/schema name (mysql / postgres / mongo).
    Database(String),
    /// Redis numeric db index (0..15 by default).
    RedisDb(u8),
    /// Elasticsearch index name.
    EsIndex(String),
}

#[async_trait]
pub trait DbDriver: Send + Sync {
    fn engine(&self) -> Engine;
    async fn engine_version(&self) -> anyhow::Result<String>;
    /// Idempotently create the database. No-op when it already exists.
    async fn ensure_db(&self, name: &str) -> anyhow::Result<()>;
    /// Drop every database whose name begins with `prefix`. Returns the
    /// names that were dropped.
    async fn drop_matching(&self, prefix: &str) -> anyhow::Result<Vec<String>>;
    /// List namespaces matching `prefix` (no mutation).
    async fn list_matching(&self, prefix: &str) -> anyhow::Result<Vec<String>>;
    /// Clear a namespace in place (e.g. redis FLUSHDB).
    async fn flush_namespace(&self, ns: &Namespace) -> anyhow::Result<()>;
}
