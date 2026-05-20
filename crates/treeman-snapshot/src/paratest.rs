//! Paratest fan-out: clone an already-built source DB into N parallel
//! per-process test DBs. Mirrors build_helper/clone/clone.go.
//!
//! Concurrency model: per-target worker pool (4), bounded by a global
//! semaphore so the whole fan-out caps at min(N*4, 128).

use std::sync::Arc;

use anyhow::{Context, Result};
use tokio::sync::Semaphore;

use treeman_core::template::{TemplateContext, render};
use treeman_db::mysql::MysqlDriver;
use treeman_db::postgres::PostgresDriver;

#[derive(Debug, Clone, Copy)]
pub enum Engine {
    Mysql,
    Postgres,
}

#[derive(Debug, Clone)]
pub struct ParatestPlan {
    pub engine: Engine,
    pub source_db: String,
    pub clones: u32,
    /// Template with `{slug}` and `{n}` placeholders.
    pub name_template: String,
}

/// Compute the rendered clone DB names.
pub fn render_clone_names(plan: &ParatestPlan, slug: &str) -> Result<Vec<String>> {
    let base_ctx = TemplateContext::from_slug(&treeman_core::slug::Slug {
        value: slug.into(),
        source: treeman_core::slug::SlugSource::Ticket,
    });
    let mut out = Vec::with_capacity(plan.clones as usize);
    for n in 1..=plan.clones {
        let ctx = base_ctx.clone().with_n(n);
        out.push(render(&plan.name_template, &ctx)?);
    }
    Ok(out)
}

/// Auto = num_cpus.
pub fn auto_clones() -> u32 {
    std::thread::available_parallelism()
        .map(|n| n.get() as u32)
        .unwrap_or(4)
}

pub async fn mysql_fanout(
    driver: Arc<MysqlDriver>,
    plan: ParatestPlan,
    slug: &str,
) -> Result<Vec<String>> {
    let names = render_clone_names(&plan, slug)?;
    let semaphore = Arc::new(Semaphore::new(global_concurrency(plan.clones)));
    let mut handles = Vec::with_capacity(names.len());
    for name in &names {
        let sem = semaphore.clone();
        let drv = driver.clone();
        let src = plan.source_db.clone();
        let dst = name.clone();
        let h = tokio::spawn(async move {
            let _permit = sem.acquire().await.expect("permit");
            drv.snapshot_create(&src, &dst)
                .await
                .with_context(|| format!("mysql clone {src} → {dst}"))
        });
        handles.push(h);
    }
    for h in handles {
        h.await??;
    }
    Ok(names)
}

pub async fn postgres_fanout(
    driver: Arc<PostgresDriver>,
    plan: ParatestPlan,
    slug: &str,
) -> Result<Vec<String>> {
    let names = render_clone_names(&plan, slug)?;
    let semaphore = Arc::new(Semaphore::new(global_concurrency(plan.clones)));
    let mut handles = Vec::with_capacity(names.len());
    for name in &names {
        let sem = semaphore.clone();
        let drv = driver.clone();
        let src = plan.source_db.clone();
        let dst = name.clone();
        let h = tokio::spawn(async move {
            let _permit = sem.acquire().await.expect("permit");
            drv.snapshot_restore(&src, &dst)
                .await
                .with_context(|| format!("pg clone {src} → {dst}"))
        });
        handles.push(h);
    }
    for h in handles {
        h.await??;
    }
    Ok(names)
}

/// min(clones*4, 128) — mirrors clone.go's clamp.
fn global_concurrency(clones: u32) -> usize {
    (clones as usize).saturating_mul(4).clamp(4, 128)
}
