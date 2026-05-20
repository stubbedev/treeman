//! Daemon-side state: shared SQLite pool, currently-running watchers
//! keyed by canonical repo path.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Mutex;

use anyhow::{Context, Result};
use sqlx::SqlitePool;
use tokio::sync::mpsc;
use tokio::task::JoinHandle;
use tracing::info;
use treeman_migrations::Registry;

pub struct WatcherEntry {
    handles: Vec<JoinHandle<()>>,
    forward: JoinHandle<()>,
}

pub struct DaemonState {
    pool: SqlitePool,
    pub started_at_unix: i64,
    pub pid: u32,
    watchers: Mutex<HashMap<String, WatcherEntry>>,
}

impl DaemonState {
    pub fn new(pool: SqlitePool, started_at_unix: i64, pid: u32) -> Self {
        Self {
            pool,
            started_at_unix,
            pid,
            watchers: Default::default(),
        }
    }
    pub fn sqlite(&self) -> &SqlitePool {
        &self.pool
    }
    pub fn watcher_count(&self) -> usize {
        self.watchers.lock().unwrap().len()
    }
    pub fn list_watchers(&self) -> Vec<String> {
        let g = self.watchers.lock().unwrap();
        let mut k: Vec<String> = g.keys().cloned().collect();
        k.sort();
        k
    }

    pub async fn start_watcher(&self, repo_path: &str) -> Result<()> {
        let canonical = PathBuf::from(repo_path)
            .canonicalize()
            .with_context(|| format!("canonicalize {repo_path}"))?;
        let key = canonical.to_string_lossy().to_string();
        if self.watchers.lock().unwrap().contains_key(&key) {
            return Ok(()); // already running
        }
        let cfg = treeman_core::config::load_layered(Some(&canonical))?;
        let registry = Registry::with_builtins().merge_yaml(&cfg.frameworks);
        let detected: Vec<_> = registry
            .detect_all(&canonical)
            .into_iter()
            .cloned()
            .collect();
        if detected.is_empty() {
            anyhow::bail!("no migration frameworks detected in {key}");
        }
        let (tx, mut rx) = mpsc::channel(64);
        let handles = treeman_watcher::spawn_repo_watcher(
            canonical.clone(),
            detected,
            cfg.watcher.debounce_ms,
            tx,
        )
        .await?;
        // Forwarder task: drain dispatches and persist as events.
        let pool = self.pool.clone();
        let canonical_path = canonical.clone();
        let forward = tokio::spawn(async move {
            let repo_name = canonical_path
                .file_name()
                .and_then(|s| s.to_str())
                .unwrap_or("repo");
            let repo_id = treeman_store::ensure_repo(&pool, &canonical_path, repo_name)
                .await
                .ok();
            while let Some((framework, dispatch)) = rx.recv().await {
                let payload = serde_json::json!({
                    "framework": framework, "dispatch": &dispatch,
                })
                .to_string();
                let _ = treeman_store::write_event(
                    &pool,
                    "info",
                    "watcher_event",
                    Some(&format!("{framework}: {:?}", dispatch)),
                    repo_id,
                    None,
                    None,
                    None,
                    &payload,
                )
                .await;
                // Action: incremental delta on add-only, full prepare otherwise.
                if !matches!(dispatch, treeman_watcher::Dispatch::Noop) {
                    if let Ok(cfg) = treeman_core::config::load_layered(Some(&canonical_path)) {
                        let slug = treeman_core::slug_for(&canonical_path, None);
                        let rid = repo_id.unwrap_or(0);
                        let wt_id = treeman_store::ensure_worktree(
                            &pool,
                            rid,
                            &canonical_path,
                            &slug.value,
                            None,
                        )
                        .await
                        .unwrap_or(0);
                        let res = match dispatch {
                            treeman_watcher::Dispatch::Delta(_) => treeman_prepare::delta_run(
                                &cfg,
                                &canonical_path,
                                &slug,
                                &pool,
                                rid,
                                wt_id,
                            )
                            .await
                            .map(|_| ()),
                            _ => treeman_prepare::run(
                                &cfg,
                                &canonical_path,
                                &slug,
                                &pool,
                                rid,
                                wt_id,
                            )
                            .await
                            .map(|_| ()),
                        };
                        if let Err(e) = res {
                            let _ = treeman_store::write_event(
                                &pool,
                                "error",
                                "prepare_error",
                                Some(&e.to_string()),
                                repo_id,
                                Some(wt_id),
                                None,
                                None,
                                "{}",
                            )
                            .await;
                        }
                    }
                }
            }
        });
        let entry = WatcherEntry { handles, forward };
        self.watchers.lock().unwrap().insert(key.clone(), entry);
        info!(repo = %key, "watcher started");
        Ok(())
    }

    pub async fn stop_watcher(&self, repo_path: &str) -> Result<()> {
        let canonical = PathBuf::from(repo_path)
            .canonicalize()
            .unwrap_or_else(|_| PathBuf::from(repo_path));
        let key = canonical.to_string_lossy().to_string();
        let entry = self.watchers.lock().unwrap().remove(&key);
        if let Some(e) = entry {
            for h in e.handles {
                h.abort();
            }
            e.forward.abort();
            info!(repo = %key, "watcher stopped");
        }
        Ok(())
    }
}
