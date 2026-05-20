//! MySQL driver — sqlx::MySql. Pure wire (no `mysql` client shell-out).

use anyhow::{Context, Result};
use async_trait::async_trait;
use sqlx::Row;
use sqlx::mysql::{MySqlPool, MySqlPoolOptions};
use tracing::debug;
use treeman_core::config::MysqlConn;

use crate::{DbDriver, Engine, Namespace};

pub struct MysqlDriver {
    pool: MySqlPool,
}

impl MysqlDriver {
    pub async fn connect(cfg: &MysqlConn) -> Result<Self> {
        // Probe TCP first — surfaces "unreachable at host:port" before
        // sqlx's pool wraps a refused-connection error.
        crate::reachability::probe("mysql", &cfg.host, cfg.port).await?;

        // The resolver fills `cfg.password` from the repo's .env*
        // files at config-load time, so a daemon serving many repos
        // doesn't need any process-env credentials at all. We still
        // honor `password_env` as a fallback for global configs that
        // declared an env var directly.
        let password = cfg
            .password
            .clone()
            .or_else(|| {
                cfg.password_env
                    .as_deref()
                    .and_then(|k| std::env::var(k).ok())
            })
            .unwrap_or_default();
        // No DB scope; connection lives at the server level so DDL like
        // CREATE/DROP DATABASE works.
        let url = format!(
            "mysql://{user}:{pass}@{host}:{port}/?ssl-mode=DISABLED",
            user = cfg.user,
            pass = urlencoding::encode_no_alloc(&password),
            host = cfg.host,
            port = cfg.port,
        );
        let pool = MySqlPoolOptions::new()
            .max_connections(cfg.pool_max)
            .connect(&url)
            .await
            .context("connect mysql")?;
        Ok(Self { pool })
    }
}

#[async_trait]
impl DbDriver for MysqlDriver {
    fn engine(&self) -> Engine {
        Engine::Mysql
    }

    async fn engine_version(&self) -> Result<String> {
        let row = sqlx::query("SELECT VERSION() AS v")
            .fetch_one(&self.pool)
            .await?;
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
        sqlx::query(&stmt)
            .execute(&self.pool)
            .await
            .with_context(|| format!("CREATE DATABASE `{name}`"))?;
        debug!(db = name, "ensured mysql database");
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let matched = self.list_matching(prefix).await?;
        for name in &matched {
            validate_ident(name)?;
            let stmt = format!("DROP DATABASE IF EXISTS `{}`", name);
            sqlx::query(&stmt)
                .execute(&self.pool)
                .await
                .with_context(|| format!("DROP DATABASE `{name}`"))?;
            debug!(db = name, "dropped mysql database");
        }
        Ok(matched)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let like = format!("{}%", prefix.replace('_', "\\_"));
        // `information_schema.SCHEMATA.SCHEMA_NAME` is declared
        // `varchar(64)` but MySQL 8 returns it over the wire as
        // VARBINARY whenever the client's connection charset doesn't
        // match the metadata charset (utf8mb3 vs utf8mb4). sqlx's
        // String decoder rejects VARBINARY. Wrap with `CONVERT … USING
        // utf8mb4` so the wire type is always a stringy VARCHAR.
        let rows = sqlx::query(
            "SELECT CONVERT(SCHEMA_NAME USING utf8mb4) AS SCHEMA_NAME \
             FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ?",
        )
        .bind(&like)
        .fetch_all(&self.pool)
        .await
        .context("list mysql databases")?;
        Ok(rows
            .into_iter()
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

impl MysqlDriver {
    pub async fn acquire(&self) -> Result<sqlx::pool::PoolConnection<sqlx::MySql>> {
        Ok(self.pool.acquire().await?)
    }

    /// Cross-DB snapshot via `INSERT INTO target.t SELECT * FROM source.t`.
    /// Mirrors build_helper/clone/clone.go: DROP+CREATE target, then DDL
    /// (tables, views, triggers) cloned, then table data copied with
    /// foreign_key_checks/unique_checks/sql_log_bin disabled for the
    /// session.
    pub async fn snapshot_create(&self, source: &str, template: &str) -> Result<()> {
        validate_ident(source)?;
        validate_ident(template)?;
        // Drop + create the template DB.
        sqlx::query(&format!("DROP DATABASE IF EXISTS `{}`", template))
            .execute(&self.pool)
            .await?;
        sqlx::query(&format!(
            "CREATE DATABASE `{}` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
            template
        ))
        .execute(&self.pool)
        .await?;

        // Enumerate source tables. `CONVERT … USING utf8mb4` — see
        // the rationale on `list_matching` above. Without this, MySQL
        // 8 servers configured with a non-utf8mb4 connection charset
        // hand back VARBINARY columns and sqlx fails to decode as
        // String.
        let table_rows = sqlx::query(
            "SELECT \
                CONVERT(TABLE_NAME USING utf8mb4) AS TABLE_NAME, \
                CONVERT(TABLE_TYPE USING utf8mb4) AS TABLE_TYPE \
             FROM information_schema.TABLES \
             WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME",
        )
        .bind(source)
        .fetch_all(&self.pool)
        .await?;

        let mut conn = self.pool.acquire().await?;
        sqlx::query("SET SESSION foreign_key_checks=0")
            .execute(&mut *conn)
            .await?;
        sqlx::query("SET SESSION unique_checks=0")
            .execute(&mut *conn)
            .await?;
        sqlx::query("SET SESSION sql_log_bin=0")
            .execute(&mut *conn)
            .await
            .ok();

        for r in &table_rows {
            let table: String = r.try_get("TABLE_NAME")?;
            let ttype: String = r.try_get("TABLE_TYPE")?;
            validate_ident(&table)?;
            // Get CREATE TABLE/VIEW from source.
            let create_kind = if ttype == "VIEW" { "VIEW" } else { "TABLE" };
            let create_row =
                sqlx::query(&format!("SHOW CREATE {create_kind} `{source}`.`{table}`",))
                    .fetch_one(&mut *conn)
                    .await?;
            let create_stmt: String = create_row.try_get(1)?;
            // The DDL references the source schema implicitly — switch
            // session schema to target before executing.
            sqlx::query(&format!("USE `{}`", template))
                .execute(&mut *conn)
                .await?;
            sqlx::query(&create_stmt)
                .execute(&mut *conn)
                .await
                .with_context(|| format!("recreate `{table}`"))?;

            if ttype != "VIEW" {
                let copy = format!(
                    "INSERT INTO `{template}`.`{table}` SELECT * FROM `{source}`.`{table}`"
                );
                sqlx::query(&copy)
                    .execute(&mut *conn)
                    .await
                    .with_context(|| format!("copy data for `{table}`"))?;
            }
        }
        Ok(())
    }

    pub async fn snapshot_restore(&self, template: &str, target: &str) -> Result<()> {
        // Restore is symmetric: rebuild `target` from `template`.
        validate_ident(template)?;
        validate_ident(target)?;
        sqlx::query(&format!("DROP DATABASE IF EXISTS `{}`", target))
            .execute(&self.pool)
            .await?;
        self.snapshot_create(template, target).await
    }

    /// Drop a snapshot template database. Idempotent.
    pub async fn drop_snapshot(&self, template: &str) -> Result<()> {
        validate_ident(template)?;
        sqlx::query(&format!("DROP DATABASE IF EXISTS `{}`", template))
            .execute(&self.pool)
            .await?;
        Ok(())
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
