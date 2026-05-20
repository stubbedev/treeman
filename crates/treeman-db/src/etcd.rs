//! etcd v3 driver. Uses the gRPC-gateway HTTP `/v3/kv/*` endpoints.
//! "Database" maps to a key prefix.

use anyhow::Result;
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as B64;
use serde::Deserialize;
use serde_json::json;

use crate::http_prefix::{HttpClient, HttpEndpoint};
use crate::{DbDriver, Engine, Namespace};

#[derive(Debug, Clone)]
pub struct EtcdDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl EtcdDriver {
    pub fn connect(url: &str) -> Result<Self> {
        Ok(Self {
            ep: HttpEndpoint::new(url),
            http: HttpClient::new()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct RangeResp {
    #[serde(default)]
    kvs: Vec<Kv>,
}
#[derive(Debug, Deserialize)]
struct Kv {
    key: String,
}

fn b64(s: &str) -> String {
    B64.encode(s.as_bytes())
}

#[async_trait]
impl DbDriver for EtcdDriver {
    fn engine(&self) -> Engine {
        Engine::Etcd
    }

    async fn engine_version(&self) -> Result<String> {
        let v: serde_json::Value = self.http.get_json(&self.ep, "/version").await?;
        Ok(v["etcdserver"].as_str().unwrap_or("etcd").to_string())
    }

    async fn ensure_db(&self, _name: &str) -> Result<()> {
        // etcd has no per-database concept; key-prefix scoping is implicit.
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        // range_end is the byte-incremented prefix
        let mut range_end = prefix.as_bytes().to_vec();
        if let Some(last) = range_end.last_mut() {
            *last = last.saturating_add(1);
        }
        let body = json!({
            "key": b64(prefix),
            "range_end": B64.encode(&range_end),
        });
        let _ = self
            .http
            .post_json(&self.ep, "/v3/kv/deleterange", &body)
            .await?;
        Ok(vec![prefix.into()])
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let mut range_end = prefix.as_bytes().to_vec();
        if let Some(last) = range_end.last_mut() {
            *last = last.saturating_add(1);
        }
        let body = json!({
            "key": b64(prefix),
            "range_end": B64.encode(&range_end),
            "keys_only": true,
        });
        let resp = self.http.post_json(&self.ep, "/v3/kv/range", &body).await?;
        if !resp.status().is_success() {
            anyhow::bail!("etcd range: {}", resp.status());
        }
        let parsed: RangeResp = resp.json().await?;
        let out = parsed
            .kvs
            .into_iter()
            .filter_map(|k| B64.decode(k.key).ok())
            .map(|b| String::from_utf8_lossy(&b).into_owned())
            .collect();
        Ok(out)
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let p = match ns {
            Namespace::Database(s) | Namespace::Prefix(s) => s.clone(),
            _ => anyhow::bail!("etcd only supports Database/Prefix namespaces"),
        };
        self.drop_matching(&p).await?;
        Ok(())
    }
}
