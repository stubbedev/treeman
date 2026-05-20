//! SQLite-backed snapshot catalog. Reads/writes `snapshots` table.

use anyhow::Result;
use serde::{Deserialize, Serialize};
use sqlx::SqlitePool;

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct SnapshotRow {
    pub fingerprint: String,
    pub engine: String,
    pub engine_version: String,
    pub source_db: String,
    pub template_name: String,
    pub migrations_hash: String,
    pub dump_hash: Option<String>,
    pub lockfile_hashes_json: String,
    pub size_bytes: Option<i64>,
    pub created_at: i64,
    pub last_used_at: i64,
    pub use_count: i64,
}

pub async fn record_built(
    pool: &SqlitePool,
    key: &crate::SnapshotKey,
    template_name: &str,
    size_bytes: Option<i64>,
) -> Result<()> {
    let now = chrono::Utc::now().timestamp_millis();
    let lockfiles = serde_json::to_string(&key.lockfile_hashes)?;
    sqlx::query(
        "INSERT INTO snapshots(
            fingerprint, engine, engine_version, source_db, template_name,
            migrations_hash, dump_hash, lockfile_hashes_json, size_bytes,
            created_at, last_used_at, use_count
         ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
         ON CONFLICT(fingerprint) DO UPDATE SET
            last_used_at = excluded.last_used_at",
    )
    .bind(key.fingerprint())
    .bind(&key.engine)
    .bind(&key.engine_version)
    .bind(&key.source_db_name)
    .bind(template_name)
    .bind(&key.migrations_hash_hex)
    .bind(&key.dump_hash_hex)
    .bind(&lockfiles)
    .bind(size_bytes)
    .bind(now)
    .bind(now)
    .execute(pool).await?;
    Ok(())
}

pub async fn mark_used(pool: &SqlitePool, fingerprint: &str) -> Result<()> {
    let now = chrono::Utc::now().timestamp_millis();
    sqlx::query(
        "UPDATE snapshots SET last_used_at = ?, use_count = use_count + 1
         WHERE fingerprint = ?",
    )
    .bind(now).bind(fingerprint).execute(pool).await?;
    Ok(())
}

pub async fn list(pool: &SqlitePool, engine: Option<&str>) -> Result<Vec<SnapshotRow>> {
    if let Some(e) = engine {
        Ok(sqlx::query_as::<_, SnapshotRow>(
            "SELECT * FROM snapshots WHERE engine = ? ORDER BY last_used_at DESC",
        ).bind(e).fetch_all(pool).await?)
    } else {
        Ok(sqlx::query_as::<_, SnapshotRow>(
            "SELECT * FROM snapshots ORDER BY last_used_at DESC",
        ).fetch_all(pool).await?)
    }
}

/// LRU GC: per source_db, drop everything beyond the `keep_per_source`
/// most recent. Also expire snapshots older than `max_age_days` or once
/// the total `size_bytes` exceeds `max_total_gb`. Returns dropped rows
/// — the caller is responsible for issuing engine-side DROP DATABASE.
pub async fn gc_lru(
    pool: &SqlitePool,
    keep_per_source: u32,
    max_age_days: u32,
    max_total_gb: u32,
) -> Result<Vec<SnapshotRow>> {
    let mut to_drop: Vec<SnapshotRow> = Vec::new();
    let all = list(pool, None).await?;

    // Per-source LRU
    let mut by_source: std::collections::BTreeMap<(String, String), Vec<SnapshotRow>> =
        Default::default();
    for r in &all {
        by_source.entry((r.engine.clone(), r.source_db.clone())).or_default().push(r.clone());
    }
    for ((_, _), mut rows) in by_source {
        rows.sort_by(|a, b| b.last_used_at.cmp(&a.last_used_at));
        if rows.len() > keep_per_source as usize {
            to_drop.extend(rows.split_off(keep_per_source as usize));
        }
    }

    // Age cutoff
    if max_age_days > 0 {
        let cutoff = chrono::Utc::now().timestamp_millis()
            - (max_age_days as i64) * 86_400_000;
        for r in &all {
            if r.last_used_at < cutoff && !to_drop.iter().any(|x| x.fingerprint == r.fingerprint) {
                to_drop.push(r.clone());
            }
        }
    }

    // Total-size cap (drop oldest until under cap)
    if max_total_gb > 0 {
        let cap = max_total_gb as i64 * 1024 * 1024 * 1024;
        let mut total: i64 = all.iter().filter_map(|r| r.size_bytes).sum();
        let mut sorted = all.clone();
        sorted.sort_by(|a, b| a.last_used_at.cmp(&b.last_used_at)); // oldest first
        for r in sorted {
            if total <= cap { break; }
            if to_drop.iter().any(|x| x.fingerprint == r.fingerprint) { continue; }
            total -= r.size_bytes.unwrap_or(0);
            to_drop.push(r);
        }
    }

    for r in &to_drop {
        sqlx::query("DELETE FROM snapshots WHERE fingerprint = ?")
            .bind(&r.fingerprint).execute(pool).await?;
    }
    Ok(to_drop)
}
