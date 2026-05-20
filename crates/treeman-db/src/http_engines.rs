//! HTTP-driven engines that scope by index/collection name. Each one
//! is a thin wrapper over `http_prefix::HttpEndpoint`.
//!
//! * `MeilisearchDriver` — index-based search (`/indexes/*`).
//! * `TypesenseDriver` — collection-based search (`/collections/*`).
//! * `QdrantDriver` — vector collections (`/collections/*`).
//! * `WeaviateDriver` — vector classes (`/v1/schema/*`).
//! * `MilvusDriver` — vector collections (`/v1/collections/*`).
//! * `NatsDriver` — JetStream streams (`/jsz?streams=1`).
//! * `KafkaDriver` — admin via Kafka REST proxy (`/topics`).

use anyhow::{Context, Result};
use async_trait::async_trait;
use serde::Deserialize;

use crate::http_prefix::{HttpClient, HttpEndpoint};
use crate::{DbDriver, Engine, Namespace};

// ────────────────────────── Meilisearch ──────────────────────────

#[derive(Debug, Clone)]
pub struct MeilisearchDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl MeilisearchDriver {
    pub fn connect(url: &str, api_key: Option<&str>) -> Result<Self> {
        let mut ep = HttpEndpoint::new(url);
        if let Some(k) = api_key {
            ep = ep.with_bearer(k);
        }
        Ok(Self {
            ep,
            http: HttpClient::new()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct MeiliIndexes {
    results: Vec<MeiliIndex>,
}
#[derive(Debug, Deserialize)]
struct MeiliIndex {
    uid: String,
}

#[async_trait]
impl DbDriver for MeilisearchDriver {
    fn engine(&self) -> Engine {
        Engine::Meilisearch
    }
    async fn engine_version(&self) -> Result<String> {
        let v: serde_json::Value = self.http.get_json(&self.ep, "/version").await?;
        Ok(v["pkgVersion"]
            .as_str()
            .unwrap_or("meilisearch")
            .to_string())
    }
    async fn ensure_db(&self, name: &str) -> Result<()> {
        let body = serde_json::json!({"uid": name});
        let _ = self.http.post_json(&self.ep, "/indexes", &body).await?;
        Ok(())
    }
    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let list: MeiliIndexes = self.http.get_json(&self.ep, "/indexes").await?;
        let mut dropped = vec![];
        for ix in list.results {
            if ix.uid.starts_with(prefix) {
                self.http
                    .delete(&self.ep, &format!("/indexes/{}", ix.uid))
                    .await?;
                dropped.push(ix.uid);
            }
        }
        Ok(dropped)
    }
    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let list: MeiliIndexes = self.http.get_json(&self.ep, "/indexes").await?;
        Ok(list
            .results
            .into_iter()
            .filter(|i| i.uid.starts_with(prefix))
            .map(|i| i.uid)
            .collect())
    }
    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let name = match ns {
            Namespace::Database(s) | Namespace::Prefix(s) | Namespace::EsIndex(s) => s.clone(),
            Namespace::RedisDb(_) => anyhow::bail!("Meilisearch wants Database/Prefix/EsIndex"),
        };
        // Delete every document in the index.
        let _ = self
            .http
            .delete(&self.ep, &format!("/indexes/{name}/documents"))
            .await;
        Ok(())
    }
}

// ────────────────────────── Typesense ──────────────────────────

#[derive(Debug, Clone)]
pub struct TypesenseDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl TypesenseDriver {
    pub fn connect(url: &str, api_key: &str) -> Result<Self> {
        Ok(Self {
            ep: HttpEndpoint::new(url).with_header("X-TYPESENSE-API-KEY", api_key),
            http: HttpClient::new()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct TypesenseCollection {
    name: String,
}

#[async_trait]
impl DbDriver for TypesenseDriver {
    fn engine(&self) -> Engine {
        Engine::Typesense
    }
    async fn engine_version(&self) -> Result<String> {
        let v: serde_json::Value = self.http.get_json(&self.ep, "/health").await?;
        Ok(v.to_string())
    }
    async fn ensure_db(&self, name: &str) -> Result<()> {
        let body = serde_json::json!({"name": name, "fields": [{"name": ".*", "type": "auto"}]});
        let _ = self.http.post_json(&self.ep, "/collections", &body).await?;
        Ok(())
    }
    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let list: Vec<TypesenseCollection> = self.http.get_json(&self.ep, "/collections").await?;
        let mut dropped = vec![];
        for c in list {
            if c.name.starts_with(prefix) {
                self.http
                    .delete(&self.ep, &format!("/collections/{}", c.name))
                    .await?;
                dropped.push(c.name);
            }
        }
        Ok(dropped)
    }
    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let list: Vec<TypesenseCollection> = self.http.get_json(&self.ep, "/collections").await?;
        Ok(list
            .into_iter()
            .filter(|c| c.name.starts_with(prefix))
            .map(|c| c.name)
            .collect())
    }
    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let name = match ns {
            Namespace::Database(s) | Namespace::Prefix(s) | Namespace::EsIndex(s) => s.clone(),
            Namespace::RedisDb(_) => anyhow::bail!("Typesense wants Database/Prefix/EsIndex"),
        };
        let _ = self
            .http
            .delete(
                &self.ep,
                &format!("/collections/{name}/documents?filter_by=:*"),
            )
            .await;
        Ok(())
    }
}

// ────────────────────────── Qdrant ──────────────────────────

#[derive(Debug, Clone)]
pub struct QdrantDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl QdrantDriver {
    pub fn connect(url: &str, api_key: Option<&str>) -> Result<Self> {
        let mut ep = HttpEndpoint::new(url);
        if let Some(k) = api_key {
            ep = ep.with_header("api-key", k);
        }
        Ok(Self {
            ep,
            http: HttpClient::new()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct QdrantCollectionsResp {
    result: QdrantCollectionsResult,
}
#[derive(Debug, Deserialize)]
struct QdrantCollectionsResult {
    collections: Vec<QdrantCollectionRef>,
}
#[derive(Debug, Deserialize)]
struct QdrantCollectionRef {
    name: String,
}

#[async_trait]
impl DbDriver for QdrantDriver {
    fn engine(&self) -> Engine {
        Engine::Qdrant
    }
    async fn engine_version(&self) -> Result<String> {
        let v: serde_json::Value = self.http.get_json(&self.ep, "/").await?;
        Ok(v["version"].as_str().unwrap_or("qdrant").to_string())
    }
    async fn ensure_db(&self, _name: &str) -> Result<()> {
        // Qdrant collection creation needs a vectors config; treeman
        // doesn't try to guess — leave to the user's app.
        Ok(())
    }
    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let resp: QdrantCollectionsResp = self.http.get_json(&self.ep, "/collections").await?;
        let mut dropped = vec![];
        for c in resp.result.collections {
            if c.name.starts_with(prefix) {
                self.http
                    .delete(&self.ep, &format!("/collections/{}", c.name))
                    .await?;
                dropped.push(c.name);
            }
        }
        Ok(dropped)
    }
    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let resp: QdrantCollectionsResp = self.http.get_json(&self.ep, "/collections").await?;
        Ok(resp
            .result
            .collections
            .into_iter()
            .filter(|c| c.name.starts_with(prefix))
            .map(|c| c.name)
            .collect())
    }
    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let name = match ns {
            Namespace::Database(s) | Namespace::Prefix(s) | Namespace::EsIndex(s) => s.clone(),
            Namespace::RedisDb(_) => anyhow::bail!("Qdrant wants Database/Prefix/EsIndex"),
        };
        // Drop + recreate must be done by the caller because Qdrant
        // collections need a vectors config. Best-effort: clear points.
        let body = serde_json::json!({"filter": {"must": []}});
        let _ = self
            .http
            .post_json(
                &self.ep,
                &format!("/collections/{name}/points/delete"),
                &body,
            )
            .await;
        Ok(())
    }
}

// ────────────────────────── Weaviate ──────────────────────────

#[derive(Debug, Clone)]
pub struct WeaviateDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl WeaviateDriver {
    pub fn connect(url: &str, api_key: Option<&str>) -> Result<Self> {
        let mut ep = HttpEndpoint::new(url);
        if let Some(k) = api_key {
            ep = ep.with_bearer(k);
        }
        Ok(Self {
            ep,
            http: HttpClient::new()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct WeaviateSchema {
    classes: Option<Vec<WeaviateClass>>,
}
#[derive(Debug, Deserialize)]
struct WeaviateClass {
    class: String,
}

#[async_trait]
impl DbDriver for WeaviateDriver {
    fn engine(&self) -> Engine {
        Engine::Weaviate
    }
    async fn engine_version(&self) -> Result<String> {
        let v: serde_json::Value = self.http.get_json(&self.ep, "/v1/meta").await?;
        Ok(v["version"].as_str().unwrap_or("weaviate").to_string())
    }
    async fn ensure_db(&self, _name: &str) -> Result<()> {
        // Class definition is app-specific; skip.
        Ok(())
    }
    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let s: WeaviateSchema = self.http.get_json(&self.ep, "/v1/schema").await?;
        let mut dropped = vec![];
        for c in s.classes.unwrap_or_default() {
            if c.class.starts_with(prefix) {
                self.http
                    .delete(&self.ep, &format!("/v1/schema/{}", c.class))
                    .await?;
                dropped.push(c.class);
            }
        }
        Ok(dropped)
    }
    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let s: WeaviateSchema = self.http.get_json(&self.ep, "/v1/schema").await?;
        Ok(s.classes
            .unwrap_or_default()
            .into_iter()
            .filter(|c| c.class.starts_with(prefix))
            .map(|c| c.class)
            .collect())
    }
    async fn flush_namespace(&self, _ns: &Namespace) -> Result<()> {
        anyhow::bail!("Weaviate flush is destructive; use drop_matching + ensure_db");
    }
}

// ────────────────────────── Milvus ──────────────────────────

#[derive(Debug, Clone)]
pub struct MilvusDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl MilvusDriver {
    pub fn connect(url: &str, token: Option<&str>) -> Result<Self> {
        let mut ep = HttpEndpoint::new(url);
        if let Some(t) = token {
            ep = ep.with_bearer(t);
        }
        Ok(Self {
            ep,
            http: HttpClient::new()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct MilvusList {
    data: Option<Vec<String>>,
}

#[async_trait]
impl DbDriver for MilvusDriver {
    fn engine(&self) -> Engine {
        Engine::Milvus
    }
    async fn engine_version(&self) -> Result<String> {
        Ok("milvus".into())
    }
    async fn ensure_db(&self, _: &str) -> Result<()> {
        Ok(())
    }
    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let l: MilvusList = self.http.get_json(&self.ep, "/v1/collections").await?;
        let mut dropped = vec![];
        for n in l.data.unwrap_or_default() {
            if n.starts_with(prefix) {
                let body = serde_json::json!({"collectionName": &n});
                self.http
                    .post_json(&self.ep, "/v1/collections/drop", &body)
                    .await?;
                dropped.push(n);
            }
        }
        Ok(dropped)
    }
    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let l: MilvusList = self.http.get_json(&self.ep, "/v1/collections").await?;
        Ok(l.data
            .unwrap_or_default()
            .into_iter()
            .filter(|n| n.starts_with(prefix))
            .collect())
    }
    async fn flush_namespace(&self, _: &Namespace) -> Result<()> {
        anyhow::bail!("Milvus flush not supported via HTTP");
    }
}

// ────────────────────────── NATS JetStream ──────────────────────────

#[derive(Debug, Clone)]
pub struct NatsDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl NatsDriver {
    pub fn connect(url: &str) -> Result<Self> {
        // Default NATS monitoring port is 8222 (HTTP).
        Ok(Self {
            ep: HttpEndpoint::new(url),
            http: HttpClient::new()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct NatsJsz {
    streams: Option<Vec<NatsStream>>,
}
#[derive(Debug, Deserialize)]
struct NatsStream {
    name: String,
}

#[async_trait]
impl DbDriver for NatsDriver {
    fn engine(&self) -> Engine {
        Engine::Nats
    }
    async fn engine_version(&self) -> Result<String> {
        let v: serde_json::Value = self.http.get_json(&self.ep, "/varz").await?;
        Ok(v["version"].as_str().unwrap_or("nats").to_string())
    }
    async fn ensure_db(&self, _: &str) -> Result<()> {
        anyhow::bail!("NATS stream creation requires the management API; not supported by treeman");
    }
    async fn drop_matching(&self, _prefix: &str) -> Result<Vec<String>> {
        anyhow::bail!("NATS stream deletion requires the management API; not supported by treeman");
    }
    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let v: NatsJsz = self.http.get_json(&self.ep, "/jsz?streams=1").await?;
        Ok(v.streams
            .unwrap_or_default()
            .into_iter()
            .filter(|s| s.name.starts_with(prefix))
            .map(|s| s.name)
            .collect())
    }
    async fn flush_namespace(&self, _: &Namespace) -> Result<()> {
        anyhow::bail!("NATS purge requires the management API; not supported by treeman");
    }
}

// ────────────────────────── Kafka (REST Proxy) ──────────────────────────

#[derive(Debug, Clone)]
pub struct KafkaDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl KafkaDriver {
    pub fn connect(rest_proxy_url: &str) -> Result<Self> {
        Ok(Self {
            ep: HttpEndpoint::new(rest_proxy_url),
            http: HttpClient::new()?,
        })
    }
}

#[async_trait]
impl DbDriver for KafkaDriver {
    fn engine(&self) -> Engine {
        Engine::Kafka
    }
    async fn engine_version(&self) -> Result<String> {
        Ok("kafka-rest".into())
    }
    async fn ensure_db(&self, _: &str) -> Result<()> {
        anyhow::bail!("Kafka topic creation needs the Admin API; not yet wired");
    }
    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let topics: Vec<String> = self
            .http
            .get_json(&self.ep, "/topics")
            .await
            .context("list kafka topics")?;
        let mut dropped = vec![];
        for t in topics {
            if t.starts_with(prefix) {
                let _ = self.http.delete(&self.ep, &format!("/topics/{t}")).await;
                dropped.push(t);
            }
        }
        Ok(dropped)
    }
    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let topics: Vec<String> = self.http.get_json(&self.ep, "/topics").await?;
        Ok(topics
            .into_iter()
            .filter(|t| t.starts_with(prefix))
            .collect())
    }
    async fn flush_namespace(&self, _: &Namespace) -> Result<()> {
        anyhow::bail!("Kafka flush is delete-and-recreate; use drop_matching + ensure_db");
    }
}
