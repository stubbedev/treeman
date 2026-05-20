//! Neo4j driver via the HTTP transactional cypher endpoint
//! (`/db/<db>/tx/commit`). `CREATE DATABASE`/`DROP DATABASE` requires
//! Neo4j 4+ Enterprise; Community Edition only has the `neo4j` and
//! `system` databases. We send all DDL against the `system` database.

use anyhow::{Context, Result};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::json;

use crate::http_prefix::{HttpClient, HttpEndpoint};
use crate::{DbDriver, Engine, Namespace};

#[derive(Debug, Clone)]
pub struct Neo4jDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl Neo4jDriver {
    pub fn connect(url: &str, user: &str, password: &str) -> Result<Self> {
        use base64::Engine as _;
        use base64::engine::general_purpose::STANDARD as B64;
        let credential = B64.encode(format!("{user}:{password}").as_bytes());
        let ep = HttpEndpoint::new(url).with_header("Authorization", format!("Basic {credential}"));
        Ok(Self {
            ep,
            http: HttpClient::new()?,
        })
    }

    async fn cypher(&self, db: &str, statement: &str) -> Result<String> {
        let body = json!({"statements": [{"statement": statement}]});
        let resp = self
            .http
            .post_json(&self.ep, &format!("/db/{db}/tx/commit"), &body)
            .await?;
        let status = resp.status();
        let txt = resp.text().await.unwrap_or_default();
        if !status.is_success() {
            anyhow::bail!("neo4j {status}: {txt}");
        }
        Ok(txt)
    }
}

#[derive(Debug, Deserialize)]
struct ShowResult {
    results: Vec<ResultBlock>,
}
#[derive(Debug, Deserialize)]
struct ResultBlock {
    data: Vec<DataRow>,
}
#[derive(Debug, Deserialize)]
struct DataRow {
    row: Vec<serde_json::Value>,
}

#[async_trait]
impl DbDriver for Neo4jDriver {
    fn engine(&self) -> Engine {
        Engine::Neo4j
    }

    async fn engine_version(&self) -> Result<String> {
        let txt = self
            .cypher(
                "system",
                "CALL dbms.components() YIELD name, versions RETURN name + ' ' + versions[0]",
            )
            .await?;
        Ok(txt)
    }

    async fn ensure_db(&self, name: &str) -> Result<()> {
        self.cypher("system", &format!("CREATE DATABASE `{name}` IF NOT EXISTS"))
            .await?;
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let names = self.list_matching(prefix).await?;
        for n in &names {
            self.cypher("system", &format!("DROP DATABASE `{n}` IF EXISTS"))
                .await?;
        }
        Ok(names)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let txt = self
            .cypher("system", "SHOW DATABASES YIELD name RETURN name")
            .await
            .context("SHOW DATABASES")?;
        let parsed: ShowResult = serde_json::from_str(&txt)?;
        let mut out = vec![];
        for r in parsed.results {
            for d in r.data {
                if let Some(s) = d.row.first().and_then(|v| v.as_str()) {
                    if s.starts_with(prefix) {
                        out.push(s.to_string());
                    }
                }
            }
        }
        Ok(out)
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let Namespace::Database(name) = ns else {
            anyhow::bail!("Neo4j only supports Namespace::Database");
        };
        // No truncate — drop + recreate via system.
        let _ = self.drop_matching(name).await?;
        self.ensure_db(name).await
    }
}
