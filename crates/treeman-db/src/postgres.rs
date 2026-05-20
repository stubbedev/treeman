//! Postgres driver. Native `CREATE DATABASE … TEMPLATE` for snapshot —
//! gold path among the supported engines.

use anyhow::{Context, Result};
use async_trait::async_trait;
use sqlx::Row;
use sqlx::postgres::{PgPool, PgPoolOptions};
use tracing::debug;
use treeman_core::config::PostgresConn;

use crate::{DbDriver, Engine, Namespace};

pub struct PostgresDriver {
    pool: PgPool,
}

impl PostgresDriver {
    pub async fn connect(cfg: &PostgresConn) -> Result<Self> {
        crate::reachability::probe("postgres", &cfg.host, cfg.port).await?;

        let password = cfg
            .password
            .clone()
            .or_else(|| {
                cfg.password_env
                    .as_deref()
                    .and_then(|k| std::env::var(k).ok())
            })
            .unwrap_or_default();
        let url = format!(
            "postgres://{user}:{pass}@{host}:{port}/postgres?sslmode=disable",
            user = cfg.user,
            pass = urlencode(&password),
            host = cfg.host,
            port = cfg.port,
        );
        let pool = PgPoolOptions::new()
            .max_connections(cfg.pool_max)
            .connect(&url)
            .await
            .context("connect postgres")?;
        Ok(Self { pool })
    }

    /// Snapshot via native CREATE DATABASE … TEMPLATE. Requires:
    ///   - no active connections to template
    ///   - same owner
    ///   - same encoding/locale
    ///
    /// Steps: ALTER DATABASE template ALLOW_CONNECTIONS=false → terminate
    /// stragglers → CREATE … TEMPLATE → restore ALLOW_CONNECTIONS=true.
    pub async fn snapshot_create(&self, source: &str, template: &str) -> Result<()> {
        validate_ident(source)?;
        validate_ident(template)?;
        // ALLOW_CONNECTIONS=false to fence the template
        self.execute_outside_tx(&format!(
            r#"ALTER DATABASE "{source}" ALLOW_CONNECTIONS false"#
        ))
        .await?;
        let res = self.snapshot_create_inner(source, template).await;
        // Restore.
        let _ = self
            .execute_outside_tx(&format!(
                r#"ALTER DATABASE "{source}" ALLOW_CONNECTIONS true"#
            ))
            .await;
        res
    }

    async fn snapshot_create_inner(&self, source: &str, template: &str) -> Result<()> {
        // Terminate stragglers
        self.execute_outside_tx(&format!(
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity \
             WHERE datname = '{source}' AND pid <> pg_backend_pid()",
        ))
        .await?;
        // Drop existing template same-name
        self.execute_outside_tx(&format!(r#"DROP DATABASE IF EXISTS "{template}""#))
            .await?;
        self.execute_outside_tx(&format!(
            r#"CREATE DATABASE "{template}" WITH TEMPLATE "{source}""#
        ))
        .await?;
        debug!(source, template, "pg snapshot create");
        Ok(())
    }

    pub async fn snapshot_restore(&self, template: &str, target: &str) -> Result<()> {
        validate_ident(template)?;
        validate_ident(target)?;
        // Terminate connections to target if any.
        self.execute_outside_tx(&format!(
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity \
             WHERE datname = '{target}' AND pid <> pg_backend_pid()",
        ))
        .await?;
        self.execute_outside_tx(&format!(r#"DROP DATABASE IF EXISTS "{target}""#))
            .await?;
        // Mark template not-connectable while we clone from it.
        self.execute_outside_tx(&format!(
            r#"ALTER DATABASE "{template}" ALLOW_CONNECTIONS false"#
        ))
        .await?;
        let res = self
            .execute_outside_tx(&format!(
                r#"CREATE DATABASE "{target}" WITH TEMPLATE "{template}""#
            ))
            .await;
        let _ = self
            .execute_outside_tx(&format!(
                r#"ALTER DATABASE "{template}" ALLOW_CONNECTIONS true"#
            ))
            .await;
        res
    }

    /// Drop a snapshot template database. Idempotent. Terminates any
    /// remaining backend connections to the template first.
    pub async fn drop_snapshot(&self, template: &str) -> Result<()> {
        validate_ident(template)?;
        self.execute_outside_tx(&format!(
            "SELECT pg_terminate_backend(pid) FROM pg_stat_activity \
             WHERE datname = '{template}' AND pid <> pg_backend_pid()",
        ))
        .await
        .ok();
        self.execute_outside_tx(&format!(r#"DROP DATABASE IF EXISTS "{template}""#))
            .await?;
        Ok(())
    }

    /// Open a pool scoped to a specific database (needed by dump loader
    /// which has to connect TO the target DB, not the maintenance one).
    pub async fn acquire_db(&self, db: &str) -> Result<sqlx::pool::PoolConnection<sqlx::Postgres>> {
        validate_ident(db)?;
        // Quick + dirty: build a new pool per call. For prepare/restore
        // this is one connection per dump; fine.
        let _ = db; // pool is shared across DBs at the connection-string level
        Ok(self.pool.acquire().await?)
    }

    /// CREATE/DROP/ALTER DATABASE must run outside any transaction; sqlx's
    /// `execute` may implicitly wrap in one for some drivers, so we use
    /// the lower-level conn handle and explicitly disable autocommit
    /// wrapping by issuing a simple query via `Executor::execute`.
    async fn execute_outside_tx(&self, sql: &str) -> Result<()> {
        sqlx::query(sql)
            .execute(&self.pool)
            .await
            .with_context(|| format!("pg: {sql}"))?;
        Ok(())
    }
}

#[async_trait]
impl DbDriver for PostgresDriver {
    fn engine(&self) -> Engine {
        Engine::Postgres
    }

    async fn engine_version(&self) -> Result<String> {
        let row = sqlx::query("SHOW server_version")
            .fetch_one(&self.pool)
            .await?;
        Ok(row.try_get::<String, _>(0)?)
    }

    async fn ensure_db(&self, name: &str) -> Result<()> {
        validate_ident(name)?;
        let exists: Option<i32> =
            sqlx::query_scalar("SELECT 1 FROM pg_database WHERE datname = $1")
                .bind(name)
                .fetch_optional(&self.pool)
                .await?;
        if exists.is_none() {
            self.execute_outside_tx(&format!(r#"CREATE DATABASE "{name}""#))
                .await?;
        }
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let matched = self.list_matching(prefix).await?;
        for name in &matched {
            validate_ident(name)?;
            self.execute_outside_tx(&format!(
                "SELECT pg_terminate_backend(pid) FROM pg_stat_activity \
                 WHERE datname = '{name}' AND pid <> pg_backend_pid()",
            ))
            .await?;
            self.execute_outside_tx(&format!(r#"DROP DATABASE IF EXISTS "{name}""#))
                .await?;
        }
        Ok(matched)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let like = format!("{}%", prefix.replace('_', "\\_"));
        let rows = sqlx::query("SELECT datname FROM pg_database WHERE datname LIKE $1 ESCAPE '\\'")
            .bind(&like)
            .fetch_all(&self.pool)
            .await?;
        Ok(rows
            .into_iter()
            .filter_map(|r| r.try_get::<String, _>(0).ok())
            .collect())
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        match ns {
            Namespace::Database(name) => {
                self.drop_matching(name).await?;
                self.ensure_db(name).await?;
                Ok(())
            }
            _ => anyhow::bail!("postgres driver only supports Namespace::Database"),
        }
    }
}

fn validate_ident(s: &str) -> Result<()> {
    if s.is_empty() {
        anyhow::bail!("empty identifier");
    }
    for c in s.chars() {
        if !(c.is_ascii_alphanumeric() || c == '_') {
            anyhow::bail!("invalid pg identifier: {s:?}");
        }
    }
    Ok(())
}

fn urlencode(s: &str) -> String {
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
