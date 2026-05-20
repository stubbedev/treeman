//! InfluxDB 2.x driver via the `/api/v2/buckets` HTTP API. Bucket =
//! namespace.

use anyhow::{Context, Result};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::json;

use crate::http_prefix::{HttpClient, HttpEndpoint};
use crate::{DbDriver, Engine, Namespace};

#[derive(Debug, Clone)]
pub struct InfluxdbDriver {
    ep: HttpEndpoint,
    http: HttpClient,
    org_id: String,
}

impl InfluxdbDriver {
    pub fn connect(url: &str, token: &str, org_id: &str) -> Result<Self> {
        Ok(Self {
            ep: HttpEndpoint::new(url).with_header("Authorization", format!("Token {token}")),
            http: HttpClient::new()?,
            org_id: org_id.into(),
        })
    }
}

#[derive(Debug, Deserialize)]
struct Buckets {
    #[serde(default)]
    buckets: Vec<Bucket>,
}
#[derive(Debug, Deserialize)]
struct Bucket {
    id: String,
    name: String,
}

#[async_trait]
impl DbDriver for InfluxdbDriver {
    fn engine(&self) -> Engine {
        Engine::Influxdb
    }

    async fn engine_version(&self) -> Result<String> {
        Ok("influxdb2".into())
    }

    async fn ensure_db(&self, name: &str) -> Result<()> {
        let body = json!({
            "name": name,
            "orgID": self.org_id,
        });
        let resp = self
            .http
            .post_json(&self.ep, "/api/v2/buckets", &body)
            .await?;
        if !resp.status().is_success() && resp.status().as_u16() != 422 {
            anyhow::bail!("influxdb create bucket: {}", resp.status());
        }
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let list: Buckets = self
            .http
            .get_json(&self.ep, &format!("/api/v2/buckets?orgID={}", self.org_id))
            .await
            .context("list buckets")?;
        let mut dropped = vec![];
        for b in list.buckets {
            if b.name.starts_with(prefix) {
                self.http
                    .delete(&self.ep, &format!("/api/v2/buckets/{}", b.id))
                    .await?;
                dropped.push(b.name);
            }
        }
        Ok(dropped)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let list: Buckets = self
            .http
            .get_json(&self.ep, &format!("/api/v2/buckets?orgID={}", self.org_id))
            .await?;
        Ok(list
            .buckets
            .into_iter()
            .filter(|b| b.name.starts_with(prefix))
            .map(|b| b.name)
            .collect())
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let Namespace::Database(name) = ns else {
            anyhow::bail!("InfluxDB only supports Namespace::Database");
        };
        // Drop + recreate.
        let _ = self.drop_matching(name).await?;
        self.ensure_db(name).await
    }
}
