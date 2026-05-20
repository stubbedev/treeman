//! Pure-wire database drivers. No shelling out to mysql/psql/mongosh/etc.

pub mod binlog;
pub mod clickhouse;
pub mod duckdb_driver;
pub mod dumpload;
pub mod elasticsearch;
pub mod etcd;
pub mod http_prefix;
pub mod influxdb;
pub mod memcached;
pub mod mongo;
pub mod mysql;
pub mod neo4j;
pub mod postgres;
pub mod rabbitmq;
pub mod redis_driver;
pub mod s3;

use async_trait::async_trait;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Engine {
    Mysql,
    Mariadb,
    Tidb,
    Postgres,
    Cockroach,
    Mongodb,
    Redis,
    Elasticsearch,
    Opensearch,
    Sqlite,
    Duckdb,
    Clickhouse,
    Meilisearch,
    Typesense,
    Qdrant,
    Weaviate,
    Milvus,
    Neo4j,
    Influxdb,
    Memcached,
    Rabbitmq,
    Nats,
    Etcd,
    Kafka,
    S3,
}

impl Engine {
    pub fn as_str(self) -> &'static str {
        match self {
            Engine::Mysql => "mysql",
            Engine::Mariadb => "mariadb",
            Engine::Tidb => "tidb",
            Engine::Postgres => "postgres",
            Engine::Cockroach => "cockroach",
            Engine::Mongodb => "mongodb",
            Engine::Redis => "redis",
            Engine::Elasticsearch => "elasticsearch",
            Engine::Opensearch => "opensearch",
            Engine::Sqlite => "sqlite",
            Engine::Duckdb => "duckdb",
            Engine::Clickhouse => "clickhouse",
            Engine::Meilisearch => "meilisearch",
            Engine::Typesense => "typesense",
            Engine::Qdrant => "qdrant",
            Engine::Weaviate => "weaviate",
            Engine::Milvus => "milvus",
            Engine::Neo4j => "neo4j",
            Engine::Influxdb => "influxdb",
            Engine::Memcached => "memcached",
            Engine::Rabbitmq => "rabbitmq",
            Engine::Nats => "nats",
            Engine::Etcd => "etcd",
            Engine::Kafka => "kafka",
            Engine::S3 => "s3",
        }
    }
}

#[derive(Debug, Clone)]
pub enum Namespace {
    /// Top-level database/schema name (mysql / postgres / mongo / etc.).
    Database(String),
    /// Redis numeric db index (0..15 by default).
    RedisDb(u8),
    /// Elasticsearch index name (also used for ES-like search engines).
    EsIndex(String),
    /// A key/object/topic prefix used by KV stores, queues, object
    /// stores, and search engines that scope per-prefix.
    Prefix(String),
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
