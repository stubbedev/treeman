//! Elasticsearch driver. Pure HTTP via reqwest. _clone (7.4+) for
//! snapshot, _reindex fallback.

use anyhow::{Context, Result, anyhow};
use async_trait::async_trait;
use serde_json::{Value, json};
use tracing::debug;
use treeman_core::config::EsConn;

use crate::{DbDriver, Engine, Namespace};

pub struct ElasticsearchDriver {
    base: String,
    http: reqwest::Client,
}

impl ElasticsearchDriver {
    pub fn connect(cfg: &EsConn) -> Result<Self> {
        let http = reqwest::Client::builder().build().context("reqwest client")?;
        Ok(Self {
            base: cfg.url.trim_end_matches('/').to_string(),
            http,
        })
    }

    fn url(&self, path: &str) -> String {
        format!("{}/{}", self.base, path.trim_start_matches('/'))
    }

    async fn put(&self, path: &str, body: &Value) -> Result<Value> {
        let res = self.http.put(self.url(path)).json(body).send().await?;
        let s = res.status();
        let v: Value = res.json().await.unwrap_or(json!({}));
        if !s.is_success() { return Err(anyhow!("PUT {path} → HTTP {s}: {v}")); }
        Ok(v)
    }
    async fn post(&self, path: &str, body: &Value) -> Result<Value> {
        let res = self.http.post(self.url(path)).json(body).send().await?;
        let s = res.status();
        let v: Value = res.json().await.unwrap_or(json!({}));
        if !s.is_success() { return Err(anyhow!("POST {path} → HTTP {s}: {v}")); }
        Ok(v)
    }
    async fn delete(&self, path: &str) -> Result<reqwest::StatusCode> {
        let res = self.http.delete(self.url(path)).send().await?;
        Ok(res.status())
    }
    async fn get_text(&self, path: &str) -> Result<String> {
        let res = self.http.get(self.url(path)).send().await?;
        Ok(res.text().await?)
    }

    pub async fn snapshot_create(&self, source: &str, template: &str) -> Result<()> {
        validate_index(source)?;
        validate_index(template)?;
        // _clone requires source index to be read-only.
        let _ = self.put(&format!("{source}/_settings"),
            &json!({"index.blocks.write": true})).await;
        let res = self.put(&format!("{source}/_clone/{template}"),
            &json!({})).await;
        // Always try to unset the block, even on error.
        let _ = self.put(&format!("{source}/_settings"),
            &json!({"index.blocks.write": false})).await;
        match res {
            Ok(_) => { debug!(source, template, "es snapshot _clone ok"); Ok(()) }
            Err(e) => {
                // Fallback to _reindex.
                debug!(error = %e, "es _clone failed, falling back to _reindex");
                self.put(template, &json!({})).await?;
                let task = self.post("_reindex?wait_for_completion=true",
                    &json!({"source": {"index": source}, "dest": {"index": template}})).await?;
                debug!(task = %task, "es _reindex done");
                Ok(())
            }
        }
    }

    pub async fn snapshot_restore(&self, template: &str, target: &str) -> Result<()> {
        validate_index(template)?;
        validate_index(target)?;
        let _ = self.delete(target).await;
        self.snapshot_create(template, target).await
    }
}

#[async_trait]
impl DbDriver for ElasticsearchDriver {
    fn engine(&self) -> Engine { Engine::Elasticsearch }

    async fn engine_version(&self) -> Result<String> {
        let body: Value = self.http.get(self.url("/")).send().await?.json().await?;
        Ok(body.get("version").and_then(|v| v.get("number"))
            .and_then(|v| v.as_str()).unwrap_or("unknown").to_string())
    }

    async fn ensure_db(&self, name: &str) -> Result<()> {
        validate_index(name)?;
        let res = self.http.head(self.url(name)).send().await?;
        if res.status().as_u16() == 404 {
            self.put(name, &json!({})).await?;
        }
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let names = self.list_matching(prefix).await?;
        for n in &names {
            validate_index(n)?;
            let s = self.delete(n).await?;
            if !(s.is_success() || s.as_u16() == 404) {
                anyhow::bail!("DELETE {n} → HTTP {s}");
            }
        }
        Ok(names)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let body = self.get_text(&format!("_cat/indices/{prefix}*?h=index")).await?;
        Ok(body.lines().map(|l| l.trim().to_string()).filter(|l| !l.is_empty()).collect())
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        match ns {
            Namespace::EsIndex(name) | Namespace::Database(name) => {
                self.drop_matching(name).await?;
                self.ensure_db(name).await?;
                Ok(())
            }
            _ => anyhow::bail!("elasticsearch driver does not support this namespace"),
        }
    }
}

fn validate_index(name: &str) -> Result<()> {
    if name.is_empty() {
        anyhow::bail!("empty index name");
    }
    for c in name.chars() {
        // ES rejects: \, /, *, ?, ", <, >, |, space, comma, # and uppercase.
        if c.is_uppercase() || matches!(c, '\\'|'/'|'*'|'?'|'"'|'<'|'>'|'|'|','|'#'|' ') {
            anyhow::bail!("invalid es index name: {name:?}");
        }
    }
    Ok(())
}
