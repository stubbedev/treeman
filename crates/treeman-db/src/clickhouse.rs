//! ClickHouse driver over the native HTTP interface (`POST /?query=…`).
//! Database-level CRUD. Snapshot/restore not yet implemented — ClickHouse
//! cluster CLONE TABLE requires server-side configuration.

use anyhow::{Context, Result};
use async_trait::async_trait;

use crate::http_prefix::{HttpClient, HttpEndpoint};
use crate::{DbDriver, Engine, Namespace};

#[derive(Debug, Clone)]
pub struct ClickhouseDriver {
    ep: HttpEndpoint,
    http: HttpClient,
}

impl ClickhouseDriver {
    pub fn connect(url: &str, user: Option<&str>, password: Option<&str>) -> Result<Self> {
        let mut ep = HttpEndpoint::new(url);
        if let (Some(u), Some(p)) = (user, password) {
            ep = ep.with_header("X-ClickHouse-User", u);
            ep = ep.with_header("X-ClickHouse-Key", p);
        } else if let Some(u) = user {
            ep = ep.with_header("X-ClickHouse-User", u);
        }
        Ok(Self {
            ep,
            http: HttpClient::new()?,
        })
    }

    async fn execute(&self, sql: &str) -> Result<String> {
        let resp = self
            .ep
            .apply(self.http.raw().post(self.ep.url("/")).body(sql.to_string()))
            .send()
            .await
            .with_context(|| format!("ClickHouse POST {sql}"))?;
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        if !status.is_success() {
            anyhow::bail!("ClickHouse {status}: {body}");
        }
        Ok(body)
    }
}

#[async_trait]
impl DbDriver for ClickhouseDriver {
    fn engine(&self) -> Engine {
        Engine::Clickhouse
    }

    async fn engine_version(&self) -> Result<String> {
        let v = self.execute("SELECT version()").await?;
        Ok(v.trim().to_string())
    }

    async fn ensure_db(&self, name: &str) -> Result<()> {
        validate_ident(name)?;
        self.execute(&format!("CREATE DATABASE IF NOT EXISTS `{name}`"))
            .await?;
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        validate_ident(prefix)?;
        let body = self
            .execute(&format!(
                "SELECT name FROM system.databases WHERE name LIKE '{prefix}%' FORMAT TabSeparated"
            ))
            .await?;
        let names: Vec<String> = body
            .split('\n')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty() && s.starts_with(prefix))
            .collect();
        for n in &names {
            self.execute(&format!("DROP DATABASE IF EXISTS `{n}`"))
                .await?;
        }
        Ok(names)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        validate_ident(prefix)?;
        let body = self
            .execute(&format!(
                "SELECT name FROM system.databases WHERE name LIKE '{prefix}%' FORMAT TabSeparated"
            ))
            .await?;
        Ok(body
            .split('\n')
            .map(|s| s.trim().to_string())
            .filter(|s| !s.is_empty())
            .collect())
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let Namespace::Database(name) = ns else {
            anyhow::bail!("ClickHouse only supports Namespace::Database");
        };
        validate_ident(name)?;
        // No native "truncate database"; iterate tables and TRUNCATE each.
        let tables = self
            .execute(&format!(
                "SELECT name FROM system.tables WHERE database='{name}' FORMAT TabSeparated"
            ))
            .await?;
        for t in tables.split('\n').map(str::trim).filter(|s| !s.is_empty()) {
            self.execute(&format!("TRUNCATE TABLE IF EXISTS `{name}`.`{t}`"))
                .await?;
        }
        Ok(())
    }
}

fn validate_ident(s: &str) -> Result<()> {
    if !s
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-')
    {
        anyhow::bail!("invalid ClickHouse identifier: {s:?}");
    }
    Ok(())
}
