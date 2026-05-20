//! Shared HTTP-driven "namespace prefix" driver primitives. Used by
//! search engines (Meilisearch, Typesense, Qdrant, Weaviate, Milvus)
//! and KV/queue services (Etcd) that scope per-name. Each engine has
//! a thin module above this one wiring the relevant URL paths in.

use anyhow::{Context, Result};
use reqwest::Client;
use serde::Deserialize;

#[derive(Debug, Clone)]
pub struct HttpEndpoint {
    pub base_url: String,
    pub bearer_token: Option<String>,
    pub api_key_header: Option<(String, String)>,
}

impl HttpEndpoint {
    pub fn new(base_url: impl Into<String>) -> Self {
        Self {
            base_url: base_url.into().trim_end_matches('/').into(),
            bearer_token: None,
            api_key_header: None,
        }
    }
    pub fn with_bearer(mut self, t: impl Into<String>) -> Self {
        self.bearer_token = Some(t.into());
        self
    }
    pub fn with_header(mut self, name: impl Into<String>, value: impl Into<String>) -> Self {
        self.api_key_header = Some((name.into(), value.into()));
        self
    }

    pub fn url(&self, path: &str) -> String {
        if let Some(p) = path.strip_prefix('/') {
            format!("{}/{}", self.base_url, p)
        } else {
            format!("{}/{}", self.base_url, path)
        }
    }

    pub fn apply(&self, mut req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        if let Some(t) = &self.bearer_token {
            req = req.bearer_auth(t);
        }
        if let Some((k, v)) = &self.api_key_header {
            req = req.header(k, v);
        }
        req
    }
}

#[derive(Debug, Clone, Default)]
pub struct HttpClient {
    client: Client,
}

impl HttpClient {
    pub fn new() -> Result<Self> {
        let client = Client::builder()
            .timeout(std::time::Duration::from_secs(30))
            .build()
            .context("build reqwest client")?;
        Ok(Self { client })
    }

    pub fn raw(&self) -> &Client {
        &self.client
    }

    pub async fn get_json<T: for<'de> Deserialize<'de>>(
        &self,
        ep: &HttpEndpoint,
        path: &str,
    ) -> Result<T> {
        let url = ep.url(path);
        let resp = ep
            .apply(self.client.get(&url))
            .send()
            .await
            .with_context(|| format!("GET {url}"))?;
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        if !status.is_success() {
            anyhow::bail!("GET {url}: {status} {body}");
        }
        serde_json::from_str(&body).with_context(|| format!("decode JSON from {url}: {body}"))
    }

    pub async fn delete(&self, ep: &HttpEndpoint, path: &str) -> Result<()> {
        let url = ep.url(path);
        let resp = ep
            .apply(self.client.delete(&url))
            .send()
            .await
            .with_context(|| format!("DELETE {url}"))?;
        let status = resp.status();
        if !status.is_success() && status.as_u16() != 404 {
            let body = resp.text().await.unwrap_or_default();
            anyhow::bail!("DELETE {url}: {status} {body}");
        }
        Ok(())
    }

    pub async fn post_json<B: serde::Serialize>(
        &self,
        ep: &HttpEndpoint,
        path: &str,
        body: &B,
    ) -> Result<reqwest::Response> {
        let url = ep.url(path);
        let resp = ep
            .apply(self.client.post(&url).json(body))
            .send()
            .await
            .with_context(|| format!("POST {url}"))?;
        Ok(resp)
    }

    pub async fn put_json<B: serde::Serialize>(
        &self,
        ep: &HttpEndpoint,
        path: &str,
        body: &B,
    ) -> Result<reqwest::Response> {
        let url = ep.url(path);
        let resp = ep
            .apply(self.client.put(&url).json(body))
            .send()
            .await
            .with_context(|| format!("PUT {url}"))?;
        Ok(resp)
    }
}
