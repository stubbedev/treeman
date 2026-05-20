//! Teardown — the inverse of [`crate::run`]. Drops every per-worktree
//! database / namespace declared in the repo's `.treeman.yaml`. Used
//! by both the CLI's `wt delete` (foreground) path and the daemon's
//! `WorktreeTeardown` RPC (async path), so it must work with only
//! `Config` + the slug as input — no CLI-local types in the API.
//!
//! Errors per-engine are NEVER fatal: a missing redis container
//! shouldn't block dropping the mysql worktree DBs. We log each error
//! to the SQLite event log via `treeman_store::write_event` and move
//! on. Caller can grep `treeman logs` afterwards.

use anyhow::{Context, Result};
use sqlx::SqlitePool;

use treeman_core::config::Config;
use treeman_core::config::DatabaseConfig as DB;
use treeman_core::slug::{Slug, SlugSource};
use treeman_core::template::{TemplateContext, render};
use treeman_db::{DbDriver, Namespace};

/// Drop every per-worktree namespace declared by `cfg.databases`.
/// `slug` is the rendered slug value (e.g. `proj_1234`); `repo_id` /
/// `wt_id` are the SQLite store IDs for event-log correlation.
pub async fn teardown_databases(
    cfg: &Config,
    slug: &str,
    repo_id: i64,
    wt_id: i64,
    sqlite_pool: &SqlitePool,
) -> Result<()> {
    let ctx = TemplateContext::from_slug(&Slug {
        value: slug.into(),
        source: SlugSource::Ticket,
    });

    for d in &cfg.databases {
        let result: Result<()> = async {
            match d {
                DB::Mysql { name_template, .. } => {
                    let name = render(name_template, &ctx)?;
                    let mc = cfg
                        .connections
                        .mysql
                        .clone()
                        .context("connections.mysql not configured")?;
                    let drv = treeman_db::mysql::MysqlDriver::connect(&mc).await?;
                    let dropped = drv.drop_matching(&name).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "mysql",
                        slug,
                        &name,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Postgres { name_template, .. } => {
                    let name = render(name_template, &ctx)?;
                    let pc = cfg
                        .connections
                        .postgres
                        .clone()
                        .context("connections.postgres not configured")?;
                    let drv = treeman_db::postgres::PostgresDriver::connect(&pc).await?;
                    let dropped = drv.drop_matching(&name).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "postgres",
                        slug,
                        &name,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Mongodb { name_template } => {
                    let name = render(name_template, &ctx)?;
                    let mc = cfg
                        .connections
                        .mongodb
                        .clone()
                        .context("connections.mongodb not configured")?;
                    let drv = treeman_db::mongo::MongoDriver::connect(&mc).await?;
                    let dropped = drv.drop_matching(&name).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "mongodb",
                        slug,
                        &name,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Elasticsearch { namespaces } => {
                    let prefix = render(&namespaces.index_prefix_template, &ctx)?;
                    let ec = cfg
                        .connections
                        .elasticsearch
                        .clone()
                        .context("connections.elasticsearch not configured")?;
                    let drv = treeman_db::elasticsearch::ElasticsearchDriver::connect(&ec)?;
                    let dropped = drv.drop_matching(&prefix).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "elasticsearch",
                        slug,
                        &prefix,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Redis { namespaces } => {
                    let idx_str = render(&namespaces.db_index_template, &ctx)?;
                    let idx: u8 = idx_str.parse().context("redis db index parse")?;
                    let rc = cfg
                        .connections
                        .redis
                        .clone()
                        .context("connections.redis not configured")?;
                    let drv = treeman_db::redis_driver::RedisDriver::connect(&rc)?;
                    drv.flush_namespace(&Namespace::RedisDb(idx)).await?;
                    record(
                        sqlite_pool,
                        "db_flush",
                        "redis",
                        slug,
                        &format!("db{idx}"),
                        1,
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Mariadb { name_template, .. } | DB::Tidb { name_template, .. } => {
                    let name = render(name_template, &ctx)?;
                    let mc = cfg
                        .connections
                        .mysql
                        .clone()
                        .context("connections.mysql not configured")?;
                    let drv = treeman_db::mysql::MysqlDriver::connect(&mc).await?;
                    let dropped = drv.drop_matching(&name).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "mysql_compat",
                        slug,
                        &name,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Cockroach { name_template, .. } => {
                    let name = render(name_template, &ctx)?;
                    let pc = cfg
                        .connections
                        .postgres
                        .clone()
                        .context("connections.postgres not configured")?;
                    let drv = treeman_db::postgres::PostgresDriver::connect(&pc).await?;
                    let dropped = drv.drop_matching(&name).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "cockroach",
                        slug,
                        &name,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Opensearch { namespaces } => {
                    let prefix = render(&namespaces.index_prefix_template, &ctx)?;
                    let ec = cfg
                        .connections
                        .elasticsearch
                        .clone()
                        .context("connections.elasticsearch not configured")?;
                    let drv = treeman_db::elasticsearch::ElasticsearchDriver::connect(&ec)?;
                    let dropped = drv.drop_matching(&prefix).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "opensearch",
                        slug,
                        &prefix,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                _ => {
                    eprintln!(
                        "warn: teardown skipped for engine {:?} (wire-up pending)",
                        d
                    );
                    Ok(())
                }
            }
        }
        .await;
        if let Err(e) = result {
            eprintln!("warn: teardown failed for {:?}: {e}", d);
            let _ = treeman_store::write_event(
                sqlite_pool,
                "warn",
                "db_teardown_error",
                Some(&e.to_string()),
                Some(repo_id),
                Some(wt_id),
                None,
                None,
                "{}",
            )
            .await;
        }
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn record(
    pool: &SqlitePool,
    event_type: &str,
    engine: &str,
    slug: &str,
    target: &str,
    count: usize,
    repo_id: i64,
    wt_id: i64,
) {
    let payload = serde_json::json!({
        "engine": engine, "slug": slug, "target": target, "count": count,
    })
    .to_string();
    let _ = treeman_store::write_event(
        pool,
        "info",
        event_type,
        Some(&format!("{engine}: {target} ({count})")),
        Some(repo_id),
        Some(wt_id),
        None,
        None,
        &payload,
    )
    .await;
}
