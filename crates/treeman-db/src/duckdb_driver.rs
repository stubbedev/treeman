//! DuckDB driver. DuckDB is an embedded engine — a "database" is a
//! single file on disk. This driver operates at the filesystem level:
//!
//! * `ensure_db(name)` — `touch` the file at `<base_dir>/<name>.duckdb`.
//! * `drop_matching(prefix)` — delete files whose stem starts with prefix.
//! * `list_matching(prefix)` — scan `<base_dir>` for matching files.
//! * `flush_namespace(Database(name))` — overwrite the file with an
//!   empty header (effectively dropping all tables).
//!
//! Snapshot/restore lives at a higher layer (file copy of the .duckdb).

use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use async_trait::async_trait;

use crate::{DbDriver, Engine, Namespace};

#[derive(Debug, Clone)]
pub struct DuckdbDriver {
    base_dir: PathBuf,
}

impl DuckdbDriver {
    pub fn new(base_dir: impl AsRef<Path>) -> Result<Self> {
        let p = base_dir.as_ref().to_path_buf();
        std::fs::create_dir_all(&p)
            .with_context(|| format!("create DuckDB base dir {}", p.display()))?;
        Ok(Self { base_dir: p })
    }

    pub fn db_path(&self, name: &str) -> PathBuf {
        self.base_dir.join(format!("{name}.duckdb"))
    }
}

#[async_trait]
impl DbDriver for DuckdbDriver {
    fn engine(&self) -> Engine {
        Engine::Duckdb
    }

    async fn engine_version(&self) -> Result<String> {
        Ok("filesystem".into())
    }

    async fn ensure_db(&self, name: &str) -> Result<()> {
        let p = self.db_path(name);
        if !p.exists() {
            std::fs::File::create(&p).with_context(|| format!("create {}", p.display()))?;
        }
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let mut dropped = vec![];
        for entry in std::fs::read_dir(&self.base_dir).context("read DuckDB base dir")? {
            let entry = entry?;
            let stem = entry
                .path()
                .file_stem()
                .and_then(|s| s.to_str())
                .map(str::to_string);
            let Some(stem) = stem else { continue };
            if stem.starts_with(prefix) {
                let _ = std::fs::remove_file(entry.path());
                dropped.push(stem);
            }
        }
        Ok(dropped)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let mut out = vec![];
        for entry in std::fs::read_dir(&self.base_dir).context("read DuckDB base dir")? {
            let entry = entry?;
            let stem = entry
                .path()
                .file_stem()
                .and_then(|s| s.to_str())
                .map(str::to_string);
            let Some(stem) = stem else { continue };
            if stem.starts_with(prefix) {
                out.push(stem);
            }
        }
        Ok(out)
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        let Namespace::Database(name) = ns else {
            anyhow::bail!("DuckDB only supports Namespace::Database");
        };
        let p = self.db_path(name);
        std::fs::write(&p, b"").with_context(|| format!("truncate {}", p.display()))?;
        Ok(())
    }
}
