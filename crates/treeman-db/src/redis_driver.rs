//! Redis driver. Per-worktree scoping is by db index (0..15 default).

use anyhow::{Context, Result};
use async_trait::async_trait;
use redis::AsyncCommands;
use tracing::debug;
use treeman_core::config::RedisConn;

use crate::{DbDriver, Engine, Namespace};

pub struct RedisDriver {
    client: redis::Client,
}

impl RedisDriver {
    pub fn connect(cfg: &RedisConn) -> Result<Self> {
        let client = redis::Client::open(cfg.url.as_str()).context("redis client")?;
        Ok(Self { client })
    }

    async fn conn_db(&self, db: u8) -> Result<redis::aio::MultiplexedConnection> {
        let mut conn = self.client.get_multiplexed_async_connection().await
            .context("redis connect")?;
        redis::cmd("SELECT").arg(db as i64).query_async::<()>(&mut conn).await
            .with_context(|| format!("SELECT {db}"))?;
        Ok(conn)
    }
}

#[async_trait]
impl DbDriver for RedisDriver {
    fn engine(&self) -> Engine { Engine::Redis }

    async fn engine_version(&self) -> Result<String> {
        let mut conn = self.client.get_multiplexed_async_connection().await?;
        let info: String = redis::cmd("INFO").arg("server").query_async(&mut conn).await?;
        let v = info.lines()
            .find_map(|l| l.strip_prefix("redis_version:"))
            .map(|s| s.trim().to_string())
            .unwrap_or_else(|| "unknown".into());
        Ok(v)
    }

    async fn ensure_db(&self, _name: &str) -> Result<()> {
        // No-op: redis db indices always exist.
        Ok(())
    }

    async fn drop_matching(&self, _prefix: &str) -> Result<Vec<String>> {
        anyhow::bail!("redis driver: use flush_namespace(RedisDb(n)) — there are no named databases")
    }

    async fn list_matching(&self, _prefix: &str) -> Result<Vec<String>> {
        // Return the populated db indices as strings.
        let mut conn = self.client.get_multiplexed_async_connection().await?;
        let info: String = redis::cmd("INFO").arg("keyspace").query_async(&mut conn).await?;
        let mut out = vec![];
        for l in info.lines() {
            if let Some(rest) = l.strip_prefix("db") {
                if let Some((idx, _)) = rest.split_once(':') {
                    out.push(idx.to_string());
                }
            }
        }
        Ok(out)
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        match ns {
            Namespace::RedisDb(idx) => {
                let mut conn = self.conn_db(*idx).await?;
                let _: () = redis::cmd("FLUSHDB").query_async(&mut conn).await
                    .with_context(|| format!("FLUSHDB on db {idx}"))?;
                debug!(db = idx, "redis flushdb");
                Ok(())
            }
            _ => anyhow::bail!("redis driver only supports Namespace::RedisDb"),
        }
    }
}

// Suppress unused-import warning when redis crate features change.
#[allow(dead_code)]
fn _link_async_commands() {
    fn _x<T: AsyncCommands>(_: &mut T) {}
}
