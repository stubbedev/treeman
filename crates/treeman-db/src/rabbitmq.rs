//! RabbitMQ driver via the HTTP management API. "Database" maps to a
//! vhost (`/api/vhosts/<name>`); namespace ops apply per-vhost.

use anyhow::{Context, Result};
use async_trait::async_trait;
use serde::Deserialize;

use crate::http_prefix::{HttpClient, HttpEndpoint};
use crate::{DbDriver, Engine, Namespace};

#[derive(Debug, Clone)]
pub struct RabbitmqDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl RabbitmqDriver {
    pub fn connect(url: &str, user: &str, password: &str) -> Result<Self> {
        use base64::Engine as _;
        use base64::engine::general_purpose::STANDARD as B64;
        let credential = B64.encode(format!("{user}:{password}").as_bytes());
        Ok(Self {
            ep: HttpEndpoint::new(url).with_header("Authorization", format!("Basic {credential}")),
            http: HttpClient::new()?,
        })
    }
}

#[derive(Debug, Deserialize)]
struct Vhost {
    name: String,
}

#[async_trait]
impl DbDriver for RabbitmqDriver {
    fn engine(&self) -> Engine {
        Engine::Rabbitmq
    }

    async fn engine_version(&self) -> Result<String> {
        let v: serde_json::Value = self.http.get_json(&self.ep, "/api/overview").await?;
        Ok(v["rabbitmq_version"]
            .as_str()
            .unwrap_or("rabbitmq")
            .to_string())
    }

    async fn ensure_db(&self, name: &str) -> Result<()> {
        let resp = self
            .http
            .put_json(
                &self.ep,
                &format!("/api/vhosts/{}", urlencode(name)),
                &serde_json::json!({}),
            )
            .await?;
        if !resp.status().is_success() {
            anyhow::bail!("rabbitmq create vhost: {}", resp.status());
        }
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let vhs: Vec<Vhost> = self.http.get_json(&self.ep, "/api/vhosts").await?;
        let mut dropped = vec![];
        for v in vhs {
            if v.name.starts_with(prefix) {
                self.http
                    .delete(&self.ep, &format!("/api/vhosts/{}", urlencode(&v.name)))
                    .await
                    .with_context(|| format!("delete vhost {}", v.name))?;
                dropped.push(v.name);
            }
        }
        Ok(dropped)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let vhs: Vec<Vhost> = self.http.get_json(&self.ep, "/api/vhosts").await?;
        Ok(vhs
            .into_iter()
            .filter(|v| v.name.starts_with(prefix))
            .map(|v| v.name)
            .collect())
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let Namespace::Database(name) = ns else {
            anyhow::bail!("RabbitMQ only supports Namespace::Database (vhost)");
        };
        let _ = self.drop_matching(name).await?;
        self.ensure_db(name).await
    }
}

fn urlencode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}
