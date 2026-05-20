//! MySQL driver — sqlx::MySql. Pure wire (no `mysql` client shell-out).

use anyhow::{Context, Result};
use async_trait::async_trait;
use sqlx::mysql::{MySqlPool, MySqlPoolOptions};
use sqlx::Row;
use tracing::debug;
use treeman_core::config::MysqlConn;

use crate::{DbDriver, Engine, Namespace};

pub struct MysqlDriver {
    pool: MySqlPool,
}

impl MysqlDriver {
    pub async fn connect(cfg: &MysqlConn) -> Result<Self> {
        let password = cfg.password_env.as_deref()
            .map(|k| std::env::var(k).unwrap_or_default())
            .unwrap_or_default();
        // No DB scope; connection lives at the server level so DDL like
        // CREATE/DROP DATABASE works.
        let url = format!(
            "mysql://{user}:{pass}@{host}:{port}/?ssl-mode=DISABLED",
            user = cfg.user, pass = urlencoding::encode_no_alloc(&password),
            host = cfg.host, port = cfg.port,
        );
        let pool = MySqlPoolOptions::new()
            .max_connections(cfg.pool_max)
            .connect(&url).await.context("connect mysql")?;
        Ok(Self { pool })
    }
}

#[async_trait]
impl DbDriver for MysqlDriver {
    fn engine(&self) -> Engine { Engine::Mysql }

    async fn engine_version(&self) -> Result<String> {
        let row = sqlx::query("SELECT VERSION() AS v").fetch_one(&self.pool).await?;
        Ok(row.try_get::<String, _>("v")?)
    }

    async fn ensure_db(&self, name: &str) -> Result<()> {
        validate_ident(name)?;
        // CHARACTER SET / COLLATE pinned to utf8mb4 — matches what build_helper
        // / Laravel install_database.sql expects.
        let stmt = format!(
            "CREATE DATABASE IF NOT EXISTS `{}` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
            name
        );
        sqlx::query(&stmt).execute(&self.pool).await
            .with_context(|| format!("CREATE DATABASE `{name}`"))?;
        debug!(db = name, "ensured mysql database");
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let matched = self.list_matching(prefix).await?;
        for name in &matched {
            validate_ident(name)?;
            let stmt = format!("DROP DATABASE IF EXISTS `{}`", name);
            sqlx::query(&stmt).execute(&self.pool).await
                .with_context(|| format!("DROP DATABASE `{name}`"))?;
            debug!(db = name, "dropped mysql database");
        }
        Ok(matched)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let like = format!("{}%", prefix.replace('_', "\\_"));
        let rows = sqlx::query(
            "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ?"
        )
        .bind(&like)
        .fetch_all(&self.pool).await
        .context("list mysql databases")?;
        Ok(rows.into_iter()
            .filter_map(|r| r.try_get::<String, _>("SCHEMA_NAME").ok())
            .collect())
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        match ns {
            Namespace::Database(name) => {
                // For mysql, "flush" = drop+recreate. Use DROP DATABASE +
                // CREATE DATABASE to preserve the name with empty tables.
                self.drop_matching(name).await?;
                self.ensure_db(name).await?;
                Ok(())
            }
            _ => anyhow::bail!("mysql driver only supports Namespace::Database"),
        }
    }
}

/// Reject anything that isn't `[A-Za-z0-9_]+` so we can interpolate into
/// backtick-quoted identifiers safely. (sqlx prepared statements don't
/// accept identifiers as bind params, so we have to validate.)
fn validate_ident(s: &str) -> Result<()> {
    if s.is_empty() {
        anyhow::bail!("empty identifier");
    }
    for c in s.chars() {
        if !(c.is_ascii_alphanumeric() || c == '_') {
            anyhow::bail!("invalid mysql identifier: {s:?}");
        }
    }
    Ok(())
}

/// Inline urlencoding without dragging in another crate.
mod urlencoding {
    pub mod encode_no_alloc {
        pub fn _hint() {}
    }
    pub fn encode_no_alloc(s: &str) -> String {
        let mut out = String::with_capacity(s.len());
        for c in s.chars() {
            if c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '~') {
                out.push(c);
            } else {
                let mut buf = [0u8; 4];
                for b in c.encode_utf8(&mut buf).bytes() {
                    out.push_str(&format!("%{:02X}", b));
                }
            }
        }
        out
    }
}
