//! Read-side helpers: `treeman logs tail` and `treeman logs grep` use these.

use anyhow::Result;
use serde::{Deserialize, Serialize};
use sqlx::SqlitePool;

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct EventRow {
    pub id: i64,
    pub ts: i64,
    pub level: String,
    pub repo_id: Option<i64>,
    pub worktree_id: Option<i64>,
    pub event_type: String,
    pub phase: Option<String>,
    pub message: Option<String>,
    pub payload_json: String,
    pub duration_ms: Option<i64>,
}

#[derive(Debug, Clone, Default)]
pub struct EventFilter {
    pub since_ms: Option<i64>,
    pub level: Option<String>,
    pub event_type: Option<String>,
    pub worktree_id: Option<i64>,
    pub grep: Option<String>,
    pub limit: Option<i64>,
}

pub async fn query_events(pool: &SqlitePool, filter: &EventFilter) -> Result<Vec<EventRow>> {
    let mut sql = String::from(
        "SELECT id, ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms FROM events WHERE 1=1"
    );
    if filter.since_ms.is_some() { sql.push_str(" AND ts >= ?"); }
    if filter.level.is_some() { sql.push_str(" AND level = ?"); }
    if filter.event_type.is_some() { sql.push_str(" AND event_type = ?"); }
    if filter.worktree_id.is_some() { sql.push_str(" AND worktree_id = ?"); }
    if filter.grep.is_some() { sql.push_str(" AND (message LIKE ? OR payload_json LIKE ?)"); }
    sql.push_str(" ORDER BY ts DESC LIMIT ?");

    let mut q = sqlx::query_as::<_, EventRow>(&sql);
    if let Some(t) = filter.since_ms { q = q.bind(t); }
    if let Some(l) = &filter.level { q = q.bind(l); }
    if let Some(t) = &filter.event_type { q = q.bind(t); }
    if let Some(w) = filter.worktree_id { q = q.bind(w); }
    if let Some(g) = &filter.grep {
        let like = format!("%{g}%");
        q = q.bind(like.clone()).bind(like);
    }
    q = q.bind(filter.limit.unwrap_or(200));

    Ok(q.fetch_all(pool).await?)
}

/// Poll-based tail. Returns rows newer than `last_id`; caller updates
/// `last_id` on each fetch.
pub async fn tail_events(pool: &SqlitePool, last_id: i64) -> Result<Vec<EventRow>> {
    Ok(sqlx::query_as::<_, EventRow>(
        "SELECT id, ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms
         FROM events WHERE id > ? ORDER BY id ASC LIMIT 500",
    )
    .bind(last_id)
    .fetch_all(pool)
    .await?)
}
