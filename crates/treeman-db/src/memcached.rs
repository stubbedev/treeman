//! Memcached driver via the ASCII text protocol over TCP.
//! Only `flush_all` is supported as a true operation; "database" is
//! conceptual (memcached has no namespacing).

use anyhow::{Context, Result};
use async_trait::async_trait;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

use crate::{DbDriver, Engine, Namespace};

#[derive(Debug, Clone)]
pub struct MemcachedDriver {
    host: String,
    port: u16,
}

impl MemcachedDriver {
    pub fn connect(host: impl Into<String>, port: u16) -> Self {
        Self {
            host: host.into(),
            port,
        }
    }

    async fn issue(&self, cmd: &str) -> Result<String> {
        let mut sock = TcpStream::connect((self.host.as_str(), self.port))
            .await
            .with_context(|| format!("connect memcached {}:{}", self.host, self.port))?;
        sock.write_all(cmd.as_bytes()).await?;
        sock.write_all(b"\r\n").await?;
        sock.shutdown().await.ok();
        let mut buf = String::new();
        sock.read_to_string(&mut buf).await?;
        Ok(buf)
    }
}

#[async_trait]
impl DbDriver for MemcachedDriver {
    fn engine(&self) -> Engine {
        Engine::Memcached
    }

    async fn engine_version(&self) -> Result<String> {
        let r = self.issue("version").await?;
        Ok(r.lines().next().unwrap_or("memcached").to_string())
    }

    async fn ensure_db(&self, _: &str) -> Result<()> {
        Ok(())
    }

    async fn drop_matching(&self, _: &str) -> Result<Vec<String>> {
        // No native key scan; treat drop as flush_all.
        self.issue("flush_all").await?;
        Ok(vec!["flush_all".into()])
    }

    async fn list_matching(&self, _: &str) -> Result<Vec<String>> {
        Ok(vec![])
    }

    async fn flush_namespace(&self, _ns: &Namespace) -> Result<()> {
        self.issue("flush_all").await?;
        Ok(())
    }
}
