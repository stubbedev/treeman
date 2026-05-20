//! Snapshot garbage collector. Runs `gc_lru` on the sqlite catalog,
//! then drops each evicted template inside the matching DB engine.
//!
//! Connects lazily — only engines that actually have rows to drop get a
//! driver instantiated. Keeps daemon idle cost at zero.

use anyhow::{Context, Result};
use sqlx::SqlitePool;
use tracing::{info, warn};
use treeman_core::config::Config;

use crate::store::{SnapshotRow, gc_lru};

#[derive(Debug, Default, Clone)]
pub struct GcReport {
    pub catalog_evicted: usize,
    pub engine_dropped: usize,
    pub engine_failed: usize,
}

/// Run one GC pass: evict snapshots from the sqlite catalog per the policy
/// in `cfg.snapshots.retention`, then drop each evicted template from its
/// engine. Engines without connections configured are logged and skipped.
pub async fn run_gc(pool: &SqlitePool, cfg: &Config) -> Result<GcReport> {
    let r = &cfg.snapshots.retention;
    let dropped = gc_lru(pool, r.keep_per_source, r.max_age_days, r.max_total_gb).await?;
    let mut report = GcReport {
        catalog_evicted: dropped.len(),
        ..Default::default()
    };
    if dropped.is_empty() {
        return Ok(report);
    }
    info!(count = dropped.len(), "snapshot GC: evicted catalog rows");
    for row in dropped {
        match drop_template(&row, cfg).await {
            Ok(()) => report.engine_dropped += 1,
            Err(e) => {
                report.engine_failed += 1;
                warn!(
                    fingerprint = %row.fingerprint,
                    engine = %row.engine,
                    template = %row.template_name,
                    error = %e,
                    "snapshot GC: engine drop failed (catalog row still removed)"
                );
            }
        }
    }
    info!(
        evicted = report.catalog_evicted,
        dropped = report.engine_dropped,
        failed = report.engine_failed,
        "snapshot GC complete"
    );
    Ok(report)
}

async fn drop_template(row: &SnapshotRow, cfg: &Config) -> Result<()> {
    let template = &row.template_name;
    match row.engine.as_str() {
        "mysql" | "mariadb" | "tidb" => {
            let mc = cfg
                .connections
                .mysql
                .as_ref()
                .context("no mysql connection configured")?;
            let drv = treeman_db::mysql::MysqlDriver::connect(mc).await?;
            drv.drop_snapshot(template).await
        }
        "postgres" | "cockroach" => {
            let pc = cfg
                .connections
                .postgres
                .as_ref()
                .context("no postgres connection configured")?;
            let drv = treeman_db::postgres::PostgresDriver::connect(pc).await?;
            drv.drop_snapshot(template).await
        }
        "mongodb" => {
            let mc = cfg
                .connections
                .mongodb
                .as_ref()
                .context("no mongodb connection configured")?;
            let drv = treeman_db::mongo::MongoDriver::connect(mc).await?;
            drv.drop_snapshot(template).await
        }
        "elasticsearch" | "opensearch" => {
            let ec = cfg
                .connections
                .elasticsearch
                .as_ref()
                .context("no elasticsearch connection configured")?;
            let drv = treeman_db::elasticsearch::ElasticsearchDriver::connect(ec)?;
            drv.drop_snapshot(template).await
        }
        other => {
            // Engines whose snapshot strategy is fully self-contained in the
            // catalog (no engine-side artefact) — nothing more to drop.
            warn!(engine = %other, "GC: no engine-side drop path; catalog row removed only");
            Ok(())
        }
    }
}
