//! Daemon-side state: shared SQLite pool, currently-running watchers
//! keyed by canonical repo path. Per repo, we run one *worktree-index*
//! watcher that observes `.git/worktrees/` and fans out one framework
//! watcher set per linked worktree (main worktree included).

use std::collections::HashMap;
use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::{Arc, Mutex, Weak};

use anyhow::{Context, Result};
use sqlx::SqlitePool;
use tokio::sync::mpsc;
use tokio::task::JoinHandle;
use tracing::{info, warn};
use treeman_migrations::Registry;

/// Per-linked-worktree resources spawned by the fan-out loop.
struct WorktreeEntry {
    handles: Vec<JoinHandle<()>>,
    forward: JoinHandle<()>,
}

/// Per-repo resources: the index task plus one [`WorktreeEntry`] per
/// observed worktree, plus a config-file watcher that triggers a restart
/// when `.treeman.yaml` / `.treeman.local.yaml` change.
pub struct WatcherEntry {
    index: JoinHandle<()>,
    fanout: JoinHandle<()>,
    cfg_watcher: JoinHandle<()>,
    cfg_reload: JoinHandle<()>,
    per_worktree: Arc<Mutex<HashMap<PathBuf, WorktreeEntry>>>,
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
    pub fn list_watchers(&self) -> Vec<treeman_proto::WatcherSummary> {
        let g = self.watchers.lock().unwrap();
        let mut summaries: Vec<treeman_proto::WatcherSummary> = g
            .iter()
            .map(|(k, v)| treeman_proto::WatcherSummary {
                repo: k.clone(),
                worktree_count: v.per_worktree.lock().unwrap().len() as u32,
            })
            .collect();
        summaries.sort_by(|a, b| a.repo.cmp(&b.repo));
        summaries
    }

    pub fn start_watcher<'a>(
        self: &'a Arc<Self>,
        repo_path: &'a str,
    ) -> Pin<Box<dyn Future<Output = Result<()>> + Send + 'a>> {
        Box::pin(self.clone().start_watcher_impl(repo_path.to_string()))
    }

    async fn start_watcher_impl(self: Arc<Self>, repo_path: String) -> Result<()> {
        let repo_path = repo_path.as_str();
        let canonical = PathBuf::from(repo_path)
            .canonicalize()
            .with_context(|| format!("canonicalize {repo_path}"))?;
        let key = canonical.to_string_lossy().to_string();
        if self.watchers.lock().unwrap().contains_key(&key) {
            return Ok(()); // already running
        }
        let cfg = treeman_core::config::load_layered(Some(&canonical))?;
        let debounce_ms = cfg.watcher.debounce_ms;

        let (idx_tx, mut idx_rx) = mpsc::channel(16);
        let index =
            treeman_watcher::spawn_worktree_index(canonical.clone(), debounce_ms, idx_tx).await?;

        let per_worktree: Arc<Mutex<HashMap<PathBuf, WorktreeEntry>>> =
            Arc::new(Mutex::new(HashMap::new()));
        let pool = self.pool.clone();
        let main_root = canonical.clone();
        let per_worktree_task = per_worktree.clone();

        let fanout = tokio::spawn(async move {
            while let Some(diff) = idx_rx.recv().await {
                for wt in diff.removed {
                    if let Some(entry) = per_worktree_task.lock().unwrap().remove(&wt) {
                        for h in entry.handles {
                            h.abort();
                        }
                        entry.forward.abort();
                        info!(worktree = %wt.display(), "worktree watcher stopped");
                    }
                }
                for wt in diff.added {
                    if per_worktree_task.lock().unwrap().contains_key(&wt) {
                        continue;
                    }
                    match spawn_for_worktree(
                        pool.clone(),
                        main_root.clone(),
                        wt.clone(),
                        debounce_ms,
                    )
                    .await
                    {
                        Ok(Some(entry)) => {
                            per_worktree_task.lock().unwrap().insert(wt.clone(), entry);
                            info!(worktree = %wt.display(), "worktree watcher started");
                        }
                        Ok(None) => {
                            // No frameworks detected for this worktree — skip.
                        }
                        Err(e) => {
                            warn!(worktree = %wt.display(), error = %e, "spawn worktree watcher failed");
                        }
                    }
                }
            }
        });

        // Config hot-reload: watch repo-level config files plus each linked
        // worktree's `.treeman.local.yaml`. On change, restart this repo's
        // watcher with the fresh config.
        let cfg_paths = collect_config_paths(&canonical, &per_worktree.lock().unwrap());
        let (cfg_tx, mut cfg_rx) = mpsc::channel(8);
        let cfg_watcher =
            treeman_watcher::spawn_config_watcher(cfg_paths, debounce_ms, cfg_tx).await?;
        let weak: Weak<Self> = Arc::downgrade(&self);
        let repo_for_reload = canonical.to_string_lossy().to_string();
        let cfg_reload = tokio::spawn(async move {
            // Wait for the first config-change signal then trigger one
            // restart; the new watcher owns its own cfg_reload task, so we
            // exit after a single restart.
            if cfg_rx.recv().await.is_none() {
                return;
            }
            let Some(this) = weak.upgrade() else { return };
            info!(repo = %repo_for_reload, "config changed → restarting watcher");
            let _ = this.stop_watcher(&repo_for_reload).await;
            if let Err(e) = this.start_watcher(&repo_for_reload).await {
                warn!(repo = %repo_for_reload, error = %e, "restart after config change failed");
            }
        });

        let entry = WatcherEntry {
            index,
            fanout,
            cfg_watcher,
            cfg_reload,
            per_worktree,
        };
        self.watchers.lock().unwrap().insert(key.clone(), entry);
        info!(repo = %key, "watcher started");
        Ok(())
    }

    /// Return the per-worktree paths currently watched under `repo_path`.
    pub fn list_worktrees(&self, repo_path: &str) -> Result<Vec<String>> {
        let canonical = PathBuf::from(repo_path)
            .canonicalize()
            .unwrap_or_else(|_| PathBuf::from(repo_path));
        let key = canonical.to_string_lossy().to_string();
        let g = self.watchers.lock().unwrap();
        let Some(entry) = g.get(&key) else {
            anyhow::bail!("no watcher running for {key}");
        };
        let per = entry.per_worktree.lock().unwrap();
        let mut out: Vec<String> = per
            .keys()
            .map(|p| p.to_string_lossy().to_string())
            .collect();
        out.sort();
        Ok(out)
    }

    pub async fn stop_watcher(&self, repo_path: &str) -> Result<()> {
        let canonical = PathBuf::from(repo_path)
            .canonicalize()
            .unwrap_or_else(|_| PathBuf::from(repo_path));
        let key = canonical.to_string_lossy().to_string();
        let entry = self.watchers.lock().unwrap().remove(&key);
        if let Some(e) = entry {
            e.index.abort();
            e.fanout.abort();
            e.cfg_watcher.abort();
            e.cfg_reload.abort();
            let drained: Vec<WorktreeEntry> = {
                let mut g = e.per_worktree.lock().unwrap();
                g.drain().map(|(_, v)| v).collect()
            };
            for wt in drained {
                for h in wt.handles {
                    h.abort();
                }
                wt.forward.abort();
            }
            info!(repo = %key, "watcher stopped");
        }
        Ok(())
    }
}

/// Build the set of config-file paths whose changes should trigger a watcher
/// restart for a given repo.
fn collect_config_paths(
    repo_root: &PathBuf,
    per_worktree: &HashMap<PathBuf, WorktreeEntry>,
) -> Vec<PathBuf> {
    let mut paths = vec![
        repo_root.join(".treeman.yaml"),
        repo_root.join(".treeman.local.yaml"),
    ];
    if let Some(global) = treeman_core::config::global_path() {
        paths.push(global);
    }
    for wt in per_worktree.keys() {
        if wt != repo_root {
            paths.push(wt.join(".treeman.local.yaml"));
        }
    }
    paths
}

async fn spawn_for_worktree(
    pool: SqlitePool,
    main_root: PathBuf,
    wt_root: PathBuf,
    debounce_ms: u64,
) -> Result<Option<WorktreeEntry>> {
    let cfg = treeman_core::config::load_layered_for_worktree(&main_root, &wt_root)?;
    let registry = Registry::with_builtins().merge_yaml(&cfg.frameworks);
    let detected: Vec<_> = registry.detect_all(&wt_root).into_iter().cloned().collect();
    if detected.is_empty() {
        return Ok(None);
    }
    let (tx, mut rx) = mpsc::channel(64);
    let handles =
        treeman_watcher::spawn_repo_watcher(wt_root.clone(), detected, debounce_ms, tx).await?;
    let pool_fwd = pool.clone();
    let main_for_repo = main_root.clone();
    let wt_for_fwd = wt_root.clone();
    let forward = tokio::spawn(async move {
        let repo_name = main_for_repo
            .file_name()
            .and_then(|s| s.to_str())
            .unwrap_or("repo");
        let repo_id = treeman_store::ensure_repo(&pool_fwd, &main_for_repo, repo_name)
            .await
            .ok();
        while let Some((framework, dispatch)) = rx.recv().await {
            let payload = serde_json::json!({
                "framework": framework, "dispatch": &dispatch,
                "worktree": wt_for_fwd.to_string_lossy(),
            })
            .to_string();
            let _ = treeman_store::write_event(
                &pool_fwd,
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
            if !matches!(dispatch, treeman_watcher::Dispatch::Noop) {
                if let Ok(cfg) =
                    treeman_core::config::load_layered_for_worktree(&main_for_repo, &wt_for_fwd)
                {
                    let slug = treeman_core::slug_for(&wt_for_fwd, None);
                    let rid = repo_id.unwrap_or(0);
                    let wt_id = treeman_store::ensure_worktree(
                        &pool_fwd,
                        rid,
                        &wt_for_fwd,
                        &slug.value,
                        None,
                    )
                    .await
                    .unwrap_or(0);
                    let res = match dispatch {
                        treeman_watcher::Dispatch::Delta(_) => treeman_prepare::delta_run(
                            &cfg,
                            &wt_for_fwd,
                            &slug,
                            &pool_fwd,
                            rid,
                            wt_id,
                        )
                        .await
                        .map(|_| ()),
                        _ => treeman_prepare::run(&cfg, &wt_for_fwd, &slug, &pool_fwd, rid, wt_id)
                            .await
                            .map(|_| ()),
                    };
                    if let Err(e) = res {
                        let _ = treeman_store::write_event(
                            &pool_fwd,
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
    Ok(Some(WorktreeEntry { handles, forward }))
}
