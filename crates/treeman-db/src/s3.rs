//! Minimal S3/MinIO driver stub. Real signed-request support requires
//! aws-sigv4 + credential providers, which is a non-trivial pile of
//! dependencies; this stub keeps the engine dispatchable so config can
//! declare `engine: s3` today, and the real implementation will land
//! before the engine ships out of `experimental`.

use anyhow::Result;
use async_trait::async_trait;

use crate::{DbDriver, Engine, Namespace};

#[derive(Debug, Clone, Default)]
pub struct S3Driver {
    pub endpoint: String,
    pub region: String,
    pub access_key: Option<String>,
    pub secret_key: Option<String>,
}

impl S3Driver {
    pub fn connect(endpoint: &str, region: &str) -> Self {
        Self {
            endpoint: endpoint.into(),
            region: region.into(),
            ..Default::default()
        }
    }
}

const UNSUPPORTED: &str =
    "S3 driver is experimental — `treeman db` cannot mutate buckets yet (aws-sigv4 wiring pending)";

#[async_trait]
impl DbDriver for S3Driver {
    fn engine(&self) -> Engine {
        Engine::S3
    }

    async fn engine_version(&self) -> Result<String> {
        Ok("experimental".into())
    }

    async fn ensure_db(&self, _: &str) -> Result<()> {
        anyhow::bail!("{UNSUPPORTED}");
    }

    async fn drop_matching(&self, _: &str) -> Result<Vec<String>> {
        anyhow::bail!("{UNSUPPORTED}");
    }

    async fn list_matching(&self, _: &str) -> Result<Vec<String>> {
        anyhow::bail!("{UNSUPPORTED}");
    }

    async fn flush_namespace(&self, _: &Namespace) -> Result<()> {
        anyhow::bail!("{UNSUPPORTED}");
    }
}
