//! hook_runs table helpers.

use anyhow::Result;
use serde::{Deserialize, Serialize};
use sqlx::SqlitePool;

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct HookRunRow {
    pub id: i64,
    pub worktree_id: i64,
    pub phase: String,
    pub started_at: i64,
    pub finished_at: Option<i64>,
    pub exit_code: Option<i64>,
    pub stdout_tail: Option<String>,
    pub stderr_tail: Option<String>,
}

pub async fn start_hook_run(pool: &SqlitePool, worktree_id: i64, phase: &str) -> Result<i64> {
    let now = chrono::Utc::now().timestamp_millis();
    let id = sqlx::query_scalar(
        "INSERT INTO hook_runs(worktree_id, phase, started_at) VALUES (?, ?, ?) RETURNING id",
    )
    .bind(worktree_id).bind(phase).bind(now)
    .fetch_one(pool)
    .await?;
    Ok(id)
}

pub async fn finish_hook_run(
    pool: &SqlitePool,
    id: i64,
    exit_code: i32,
    stdout_tail: &str,
    stderr_tail: &str,
) -> Result<()> {
    let now = chrono::Utc::now().timestamp_millis();
    sqlx::query(
        "UPDATE hook_runs SET finished_at = ?, exit_code = ?, stdout_tail = ?, stderr_tail = ?
         WHERE id = ?",
    )
    .bind(now).bind(exit_code as i64).bind(stdout_tail).bind(stderr_tail).bind(id)
    .execute(pool)
    .await?;
    Ok(())
}

pub async fn list_worktrees(pool: &SqlitePool) -> Result<Vec<WorktreeRow>> {
    Ok(sqlx::query_as::<_, WorktreeRow>(
        "SELECT id, repo_id, path, slug, branch, created_at, deleted_at FROM worktrees
         WHERE deleted_at IS NULL ORDER BY created_at DESC",
    )
    .fetch_all(pool)
    .await?)
}

pub async fn find_worktree_by_path(pool: &SqlitePool, path: &str) -> Result<Option<WorktreeRow>> {
    Ok(sqlx::query_as::<_, WorktreeRow>(
        "SELECT id, repo_id, path, slug, branch, created_at, deleted_at FROM worktrees
         WHERE path = ?",
    )
    .bind(path)
    .fetch_optional(pool)
    .await?)
}

#[derive(Debug, Clone, Serialize, Deserialize, sqlx::FromRow)]
pub struct WorktreeRow {
    pub id: i64,
    pub repo_id: i64,
    pub path: String,
    pub slug: String,
    pub branch: Option<String>,
    pub created_at: i64,
    pub deleted_at: Option<i64>,
}
