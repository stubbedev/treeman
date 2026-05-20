//! SQLite-backed event/snapshot/worktree store. A custom
//! `tracing_subscriber::Layer` mirrors every emitted span event into the
//! `events` table so logs are queryable from the CLI without file
//! plumbing.

pub mod layer;
pub mod query;

use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use sqlx::sqlite::{SqliteConnectOptions, SqlitePoolOptions};
use sqlx::{ConnectOptions, SqlitePool};

pub use layer::{SqliteLayer, init_subscriber};
pub use query::{EventRow, query_events, tail_events};

/// Default path: `~/.local/state/treeman/treeman.db`. Honors
/// `TREEMAN_DB_PATH` if set.
pub fn default_db_path() -> Result<PathBuf> {
    if let Ok(p) = std::env::var("TREEMAN_DB_PATH") {
        return Ok(PathBuf::from(p));
    }
    let dirs = directories::ProjectDirs::from("", "", "treeman")
        .context("project dirs unavailable")?;
    let dir = dirs.data_local_dir();
    std::fs::create_dir_all(dir)
        .with_context(|| format!("create {}", dir.display()))?;
    Ok(dir.join("treeman.db"))
}

pub async fn open(path: &Path) -> Result<SqlitePool> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).ok();
    }
    let opts = SqliteConnectOptions::new()
        .filename(path)
        .create_if_missing(true)
        .journal_mode(sqlx::sqlite::SqliteJournalMode::Wal)
        .synchronous(sqlx::sqlite::SqliteSynchronous::Normal)
        .foreign_keys(true)
        .disable_statement_logging();
    let pool = SqlitePoolOptions::new()
        .max_connections(8)
        .connect_with(opts)
        .await
        .with_context(|| format!("open sqlite at {}", path.display()))?;
    migrate(&pool).await?;
    Ok(pool)
}

async fn migrate(pool: &SqlitePool) -> Result<()> {
    // sqlx::migrate! macro requires a compile-time path; use it.
    sqlx::migrate!("./migrations")
        .run(pool)
        .await
        .context("run sqlite migrations")?;
    Ok(())
}

pub async fn ensure_repo(pool: &SqlitePool, path: &Path, name: &str) -> Result<i64> {
    let path_s = path.to_string_lossy().to_string();
    let now = chrono::Utc::now().timestamp_millis();
    let id: Option<i64> = sqlx::query_scalar("SELECT id FROM repos WHERE path = ?")
        .bind(&path_s)
        .fetch_optional(pool)
        .await?;
    if let Some(id) = id {
        return Ok(id);
    }
    let id = sqlx::query_scalar(
        "INSERT INTO repos(path, name, frameworks_json, registered_at)
         VALUES (?, ?, '[]', ?) RETURNING id",
    )
    .bind(&path_s)
    .bind(name)
    .bind(now)
    .fetch_one(pool)
    .await?;
    Ok(id)
}

pub async fn ensure_worktree(
    pool: &SqlitePool,
    repo_id: i64,
    path: &Path,
    slug: &str,
    branch: Option<&str>,
) -> Result<i64> {
    let path_s = path.to_string_lossy().to_string();
    let now = chrono::Utc::now().timestamp_millis();
    let row: Option<(i64, Option<i64>)> = sqlx::query_as(
        "SELECT id, deleted_at FROM worktrees WHERE path = ?",
    )
    .bind(&path_s)
    .fetch_optional(pool)
    .await?;
    if let Some((id, deleted_at)) = row {
        if deleted_at.is_some() {
            // Re-activate.
            sqlx::query("UPDATE worktrees SET deleted_at = NULL, slug = ?, branch = ? WHERE id = ?")
                .bind(slug).bind(branch).bind(id).execute(pool).await?;
        }
        return Ok(id);
    }
    let id = sqlx::query_scalar(
        "INSERT INTO worktrees(repo_id, path, slug, branch, created_at)
         VALUES (?, ?, ?, ?, ?) RETURNING id",
    )
    .bind(repo_id).bind(&path_s).bind(slug).bind(branch).bind(now)
    .fetch_one(pool)
    .await?;
    Ok(id)
}

pub async fn mark_worktree_deleted(pool: &SqlitePool, worktree_id: i64) -> Result<()> {
    let now = chrono::Utc::now().timestamp_millis();
    sqlx::query("UPDATE worktrees SET deleted_at = ? WHERE id = ?")
        .bind(now).bind(worktree_id).execute(pool).await?;
    Ok(())
}
