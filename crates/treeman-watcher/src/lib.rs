//! File watcher: one task per detected framework, debounced events,
//! dispatches to `delta` (apply new migration only) or `rebuild` (full
//! wipe+reseed) per the table in plan §7.
//!
//! Watcher state is in `<repo>/.treeman/watch-state-<framework>.json` so
//! restarts don't trigger phantom rebuilds.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::time::Duration;

use anyhow::Result;
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;
use tracing::{debug, info};

use treeman_migrations::{FrameworkSpec, HashMode, MigrationHashSet, OnModify};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Dispatch {
    Delta(Vec<String>),  // keys of newly-added migrations
    Rebuild,
    Noop,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct WatcherState {
    pub by_key: std::collections::BTreeMap<String, String>,
    pub mode: Option<HashMode>,
}

impl WatcherState {
    fn from_hash_set(hs: &MigrationHashSet) -> Self {
        Self { by_key: hs.by_key.clone(), mode: Some(hs.mode) }
    }
}

pub fn state_path(repo_root: &Path, framework: &str) -> PathBuf {
    repo_root.join(".treeman").join(format!("watch-state-{framework}.json"))
}

pub fn load_state(path: &Path) -> Result<WatcherState> {
    if !path.exists() { return Ok(WatcherState::default()); }
    let s = std::fs::read_to_string(path)?;
    Ok(serde_json::from_str(&s).unwrap_or_default())
}

pub fn save_state(path: &Path, state: &WatcherState) -> Result<()> {
    if let Some(parent) = path.parent() { std::fs::create_dir_all(parent).ok(); }
    std::fs::write(path, serde_json::to_string_pretty(state)?)?;
    Ok(())
}

/// Compute the dispatch decision given prev + cur snapshots for one
/// framework. Pure function — testable without filesystem.
pub fn decide(prev: &WatcherState, cur: &MigrationHashSet, spec: &FrameworkSpec) -> Dispatch {
    let added: Vec<String> = cur.by_key.keys()
        .filter(|k| !prev.by_key.contains_key(*k))
        .cloned().collect();
    let removed: Vec<&String> = prev.by_key.keys()
        .filter(|k| !cur.by_key.contains_key(*k)).collect();
    let changed: Vec<&String> = prev.by_key.iter()
        .filter(|(k, v)| cur.by_key.get(*k).map(|cv| cv != *v).unwrap_or(false))
        .map(|(k, _)| k).collect();

    if added.is_empty() && removed.is_empty() && changed.is_empty() {
        return Dispatch::Noop;
    }

    match spec.hash_mode {
        HashMode::Filename => {
            if !changed.is_empty() || !removed.is_empty() {
                Dispatch::Rebuild
            } else {
                Dispatch::Delta(added)
            }
        }
        HashMode::Checksum => {
            // Removed sha + Added sha that share a basename = rename only.
            // To detect: cross-reference. We don't have filenames in the
            // hash set when mode=Checksum (keys ARE shas), so this check
            // is only meaningful with filename info — which we lack here.
            // Conservative: any change is rebuild unless on_modify=Delta
            // and added-only.
            if removed.is_empty() && changed.is_empty() {
                Dispatch::Delta(added)
            } else if spec.on_modify == OnModify::Delta {
                if added.is_empty() { Dispatch::Noop } else { Dispatch::Delta(added) }
            } else {
                Dispatch::Rebuild
            }
        }
    }
}

/// Spawn a watcher task per detected framework. Each task watches the
/// framework's migration_dirs + lockfiles; on debounced events it
/// recomputes hash_inputs, compares against the on-disk state, and emits
/// a `Dispatch` to `tx`.
pub async fn spawn_repo_watcher(
    repo_root: PathBuf,
    specs: Vec<FrameworkSpec>,
    debounce_ms: u64,
    tx: mpsc::Sender<(String, Dispatch)>,
) -> Result<Vec<tokio::task::JoinHandle<()>>> {
    let mut handles = vec![];
    for spec in specs {
        let h = spawn_one(repo_root.clone(), spec, debounce_ms, tx.clone()).await?;
        handles.push(h);
    }
    Ok(handles)
}

async fn spawn_one(
    repo_root: PathBuf,
    spec: FrameworkSpec,
    debounce_ms: u64,
    tx: mpsc::Sender<(String, Dispatch)>,
) -> Result<tokio::task::JoinHandle<()>> {
    use notify_debouncer_full::new_debouncer;

    let initial_watch_roots = spec.watch_roots(&repo_root);
    let lockfiles = spec.lockfile_paths(&repo_root);
    if initial_watch_roots.is_empty() && lockfiles.is_empty() {
        return Ok(tokio::spawn(async {}));
    }
    info!(framework = %spec.name, roots = ?initial_watch_roots, "starting watcher");

    let (raw_tx, mut raw_rx) = mpsc::channel::<()>(64);
    let raw_tx_clone = raw_tx.clone();
    let mut debouncer = new_debouncer(
        Duration::from_millis(debounce_ms),
        None,
        move |res: Result<Vec<notify_debouncer_full::DebouncedEvent>, Vec<notify::Error>>| {
            if let Ok(events) = res {
                if !events.is_empty() {
                    let _ = raw_tx_clone.try_send(());
                }
            }
        },
    )?;
    let mut watched: std::collections::HashSet<PathBuf> =
        initial_watch_roots.iter().cloned().collect();
    {
        let w = debouncer.watcher();
        use notify::Watcher;
        for d in &initial_watch_roots {
            let _ = w.watch(d, notify::RecursiveMode::Recursive);
        }
        for f in &lockfiles {
            let _ = w.watch(f, notify::RecursiveMode::NonRecursive);
        }
    }

    let state_p = state_path(&repo_root, &spec.name);
    let initial_dirs = spec.migration_dirs(&repo_root);
    let initial_files = collect_files(&spec, &initial_dirs);
    let initial_hs = spec.hash_inputs(&initial_files).unwrap_or(MigrationHashSet {
        by_key: Default::default(), mode: spec.hash_mode,
    });
    save_state(&state_p, &WatcherState::from_hash_set(&initial_hs)).ok();

    let h = tokio::spawn(async move {
        let mut debouncer = debouncer; // keep mutable; we add watches mid-run
        let mut state = WatcherState::from_hash_set(&initial_hs);
        let mut state_files_hint: HashMap<PathBuf, ()> = HashMap::new();
        while raw_rx.recv().await.is_some() {
            // 1. Dynamically expand watch coverage: re-resolve
            //    watch_roots and add any roots that weren't watched
            //    before. Handles the case where a module-pattern's
            //    parent dir (e.g. `engines/`, `app/Modules/`) didn't
            //    exist at startup and was just created.
            let cur_roots = spec.watch_roots(&repo_root);
            for r in &cur_roots {
                let already = watched.iter().any(|w| r.starts_with(w));
                if !already {
                    use notify::Watcher;
                    if debouncer.watcher().watch(r, notify::RecursiveMode::Recursive).is_ok() {
                        debug!(root = %r.display(), "added new watch root");
                        watched.insert(r.clone());
                    }
                }
            }
            // 2. Re-resolve migration_dirs (picks up new module dirs
            //    inside already-watched roots, e.g. nwidart-style
            //    `app/Modules/Foo/Database/Migrations`).
            let dirs = spec.migration_dirs(&repo_root);
            let files = collect_files(&spec, &dirs);
            for f in &files { state_files_hint.insert(f.clone(), ()); }
            let cur = match spec.hash_inputs(&files) {
                Ok(h) => h,
                Err(e) => { debug!(error = %e, "hash_inputs failed"); continue; }
            };
            let dispatch = decide(&state, &cur, &spec);
            if !matches!(dispatch, Dispatch::Noop) {
                let _ = tx.send((spec.name.clone(), dispatch.clone())).await;
                state = WatcherState::from_hash_set(&cur);
                save_state(&state_p, &state).ok();
            }
        }
    });
    Ok(h)
}

fn collect_files(spec: &FrameworkSpec, dirs: &[PathBuf]) -> Vec<PathBuf> {
    let mut all = vec![];
    for d in dirs {
        all.extend(spec.migration_files(d));
    }
    all.sort();
    all
}

#[cfg(test)]
mod tests {
    use super::*;
    use treeman_migrations::HashMode;

    fn spec(hash_mode: HashMode, on_modify: OnModify) -> FrameworkSpec {
        FrameworkSpec {
            name: "test".into(),
            markers: vec![],
            migration_dir_patterns: vec![],
            file_globs: vec!["*".into()],
            lockfiles: vec![],
            hash_mode, on_modify,
            engine_hint: None,
        }
    }
    fn hs(pairs: &[(&str, &str)], mode: HashMode) -> MigrationHashSet {
        let mut by_key = std::collections::BTreeMap::new();
        for (k, v) in pairs { by_key.insert(k.to_string(), v.to_string()); }
        MigrationHashSet { by_key, mode }
    }

    #[test]
    fn filename_new_only_is_delta() {
        let s = spec(HashMode::Filename, OnModify::Rebuild);
        let prev = WatcherState::from_hash_set(&hs(&[("a", "h1")], HashMode::Filename));
        let cur = hs(&[("a", "h1"), ("b", "h2")], HashMode::Filename);
        match decide(&prev, &cur, &s) {
            Dispatch::Delta(added) => assert_eq!(added, vec!["b".to_string()]),
            d => panic!("expected Delta, got {d:?}"),
        }
    }

    #[test]
    fn filename_change_triggers_rebuild() {
        let s = spec(HashMode::Filename, OnModify::Rebuild);
        let prev = WatcherState::from_hash_set(&hs(&[("a", "h1")], HashMode::Filename));
        let cur = hs(&[("a", "h2")], HashMode::Filename);
        assert!(matches!(decide(&prev, &cur, &s), Dispatch::Rebuild));
    }

    #[test]
    fn checksum_added_only_is_delta() {
        let s = spec(HashMode::Checksum, OnModify::Delta);
        let prev = WatcherState::from_hash_set(&hs(&[("h1", "h1")], HashMode::Checksum));
        let cur = hs(&[("h1", "h1"), ("h2", "h2")], HashMode::Checksum);
        match decide(&prev, &cur, &s) {
            Dispatch::Delta(added) => assert_eq!(added, vec!["h2".to_string()]),
            d => panic!("expected Delta, got {d:?}"),
        }
    }
}
