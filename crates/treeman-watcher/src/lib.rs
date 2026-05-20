//! File watcher: one task per detected framework, debounced events,
//! dispatches to `delta` (apply new migration only) or `rebuild` (full
//! wipe+reseed) per the table in plan §7.
//!
//! Watcher state is in `<repo>/.treeman/watch-state-<framework>.json` so
//! restarts don't trigger phantom rebuilds.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::time::Duration;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;
use tracing::{debug, info};

use treeman_migrations::{FrameworkSpec, HashMode, MigrationHashSet, OnModify};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Dispatch {
    Delta(Vec<String>), // keys of newly-added migrations
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
        Self {
            by_key: hs.by_key.clone(),
            mode: Some(hs.mode),
        }
    }
}

pub fn state_path(repo_root: &Path, framework: &str) -> PathBuf {
    repo_root
        .join(".treeman")
        .join(format!("watch-state-{framework}.json"))
}

pub fn load_state(path: &Path) -> Result<WatcherState> {
    if !path.exists() {
        return Ok(WatcherState::default());
    }
    let s = std::fs::read_to_string(path)?;
    Ok(serde_json::from_str(&s).unwrap_or_default())
}

pub fn save_state(path: &Path, state: &WatcherState) -> Result<()> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).ok();
    }
    std::fs::write(path, serde_json::to_string_pretty(state)?)?;
    Ok(())
}

/// Compute the dispatch decision given prev + cur snapshots for one
/// framework. Pure function — testable without filesystem.
pub fn decide(prev: &WatcherState, cur: &MigrationHashSet, spec: &FrameworkSpec) -> Dispatch {
    let added: Vec<String> = cur
        .by_key
        .keys()
        .filter(|k| !prev.by_key.contains_key(*k))
        .cloned()
        .collect();
    let removed: Vec<&String> = prev
        .by_key
        .keys()
        .filter(|k| !cur.by_key.contains_key(*k))
        .collect();
    let changed: Vec<&String> = prev
        .by_key
        .iter()
        .filter(|(k, v)| cur.by_key.get(*k).map(|cv| cv != *v).unwrap_or(false))
        .map(|(k, _)| k)
        .collect();

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
                if added.is_empty() {
                    Dispatch::Noop
                } else {
                    Dispatch::Delta(added)
                }
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
    let initial_hs = spec
        .hash_inputs(&initial_files)
        .unwrap_or(MigrationHashSet {
            by_key: Default::default(),
            mode: spec.hash_mode,
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
                    if debouncer
                        .watcher()
                        .watch(r, notify::RecursiveMode::Recursive)
                        .is_ok()
                    {
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
            for f in &files {
                state_files_hint.insert(f.clone(), ());
            }
            let cur = match spec.hash_inputs(&files) {
                Ok(h) => h,
                Err(e) => {
                    debug!(error = %e, "hash_inputs failed");
                    continue;
                }
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

/// Diff emitted by [`spawn_worktree_index`]. `added`/`removed` are absolute
/// worktree paths (including the main worktree on the initial snapshot).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WorktreeIndexDiff {
    pub added: Vec<PathBuf>,
    pub removed: Vec<PathBuf>,
}

/// Watch `.git/worktrees/` and emit a [`WorktreeIndexDiff`] whenever git adds
/// or removes a linked worktree. The first message emitted contains the full
/// initial set under `added` (including the main worktree). Cheap: one notify
/// handle, no polling, no subprocess.
pub async fn spawn_worktree_index(
    repo_root: PathBuf,
    debounce_ms: u64,
    tx: mpsc::Sender<WorktreeIndexDiff>,
) -> Result<tokio::task::JoinHandle<()>> {
    use notify_debouncer_full::new_debouncer;

    let git_common = resolve_git_common_dir(&repo_root)?;
    let worktrees_dir = git_common.join("worktrees");
    std::fs::create_dir_all(&worktrees_dir).ok();
    info!(dir = %worktrees_dir.display(), "starting worktree index");

    let (raw_tx, mut raw_rx) = mpsc::channel::<()>(64);
    let mut debouncer = new_debouncer(
        Duration::from_millis(debounce_ms),
        None,
        move |res: Result<Vec<notify_debouncer_full::DebouncedEvent>, Vec<notify::Error>>| {
            if let Ok(events) = res {
                if !events.is_empty() {
                    let _ = raw_tx.try_send(());
                }
            }
        },
    )?;
    {
        use notify::Watcher;
        debouncer
            .watcher()
            .watch(&worktrees_dir, notify::RecursiveMode::Recursive)?;
    }

    let initial = list_worktrees(&repo_root, &git_common);
    let mut known: std::collections::HashSet<PathBuf> = initial.iter().cloned().collect();
    let _ = tx
        .send(WorktreeIndexDiff {
            added: initial,
            removed: vec![],
        })
        .await;

    let h = tokio::spawn(async move {
        let _keep = debouncer;
        let repo = repo_root.clone();
        let common = git_common.clone();
        // After each notify burst, re-scan once immediately and again
        // after a short delay. Catches the race where `git worktree add`
        // creates `.git/worktrees/<id>/` slightly before `<id>/gitdir` is
        // written — the first scan may miss the new entry; the follow-up
        // picks it up.
        loop {
            if raw_rx.recv().await.is_none() {
                break;
            }
            // Two passes: immediate + delayed. The follow-up catches the
            // race where `git worktree add` creates `.git/worktrees/<id>/`
            // slightly before `<id>/gitdir` is written — the first scan
            // may miss the new entry.
            for delay in [Duration::from_millis(0), Duration::from_millis(300)] {
                if !delay.is_zero() {
                    tokio::time::sleep(delay).await;
                }
                let cur: std::collections::HashSet<PathBuf> =
                    list_worktrees(&repo, &common).into_iter().collect();
                let added: Vec<PathBuf> = cur.difference(&known).cloned().collect();
                let removed: Vec<PathBuf> = known.difference(&cur).cloned().collect();
                if added.is_empty() && removed.is_empty() {
                    continue;
                }
                debug!(?added, ?removed, "worktree index change");
                known = cur;
                if tx.send(WorktreeIndexDiff { added, removed }).await.is_err() {
                    return;
                }
            }
        }
    });
    Ok(h)
}

/// Watch a fixed set of config files. The watcher emits a unit message
/// every time one of `paths` is created, modified, or deleted. Non-existent
/// paths are still tracked: we recursively watch each parent directory
/// (NonRecursive) and filter events by filename. Cheap: one debouncer.
pub async fn spawn_config_watcher(
    paths: Vec<PathBuf>,
    debounce_ms: u64,
    tx: mpsc::Sender<()>,
) -> Result<tokio::task::JoinHandle<()>> {
    use notify::Watcher;
    use notify_debouncer_full::new_debouncer;

    if paths.is_empty() {
        return Ok(tokio::spawn(async {}));
    }
    let filenames: std::collections::HashSet<std::ffi::OsString> = paths
        .iter()
        .filter_map(|p| p.file_name().map(|n| n.to_os_string()))
        .collect();
    let parents: std::collections::HashSet<PathBuf> = paths
        .iter()
        .filter_map(|p| p.parent().map(|p| p.to_path_buf()))
        .collect();
    if parents.is_empty() {
        return Ok(tokio::spawn(async {}));
    }

    let (raw_tx, mut raw_rx) = mpsc::channel::<()>(16);
    let filenames_for_cb = filenames.clone();
    let mut debouncer = new_debouncer(
        Duration::from_millis(debounce_ms),
        None,
        move |res: Result<Vec<notify_debouncer_full::DebouncedEvent>, Vec<notify::Error>>| {
            let Ok(events) = res else { return };
            let relevant = events.iter().any(|ev| {
                ev.event
                    .paths
                    .iter()
                    .any(|p| p.file_name().is_some_and(|n| filenames_for_cb.contains(n)))
            });
            if relevant {
                let _ = raw_tx.try_send(());
            }
        },
    )?;
    {
        let w = debouncer.watcher();
        for p in &parents {
            std::fs::create_dir_all(p).ok();
            let _ = w.watch(p, notify::RecursiveMode::NonRecursive);
        }
    }
    info!(?paths, "starting config watcher");

    let h = tokio::spawn(async move {
        let _keep = debouncer;
        while raw_rx.recv().await.is_some() {
            if tx.send(()).await.is_err() {
                break;
            }
        }
    });
    Ok(h)
}

/// Resolve the *common* git directory for a repo root. Handles four cases:
///
/// 1. Plain repo: `<repo>/.git/` is a directory → it IS the common dir.
/// 2. Linked worktree: `<repo>/.git` is a file `gitdir: <path>` pointing into
///    `<main>/.git/worktrees/<id>/`. `commondir` marker (relative or absolute)
///    leads to `<main>/.git`.
/// 3. Submodule: `<repo>/.git` is a file pointing into
///    `<super>/.git/modules/<name>/` — no `commondir` marker because the
///    submodule has its own object store. We treat that directory as the
///    common dir directly.
/// 4. Symlinked `.git`: any of the above wrapped in a symlink. We
///    canonicalize first so the resolved path is stable.
fn resolve_git_common_dir(repo_root: &Path) -> Result<PathBuf> {
    let dot_git = repo_root.join(".git");
    let meta = std::fs::symlink_metadata(&dot_git)
        .with_context(|| format!("stat {}", dot_git.display()))?;
    if meta.file_type().is_symlink() {
        let target = std::fs::canonicalize(&dot_git)
            .with_context(|| format!("canonicalize {}", dot_git.display()))?;
        return resolve_git_common_dir_from(&target);
    }
    if meta.is_dir() {
        return Ok(dot_git.canonicalize().unwrap_or(dot_git));
    }
    if meta.is_file() {
        let raw = std::fs::read_to_string(&dot_git)?;
        let gitdir = raw.trim_start_matches("gitdir:").trim();
        let gitdir_path = if Path::new(gitdir).is_absolute() {
            PathBuf::from(gitdir)
        } else {
            repo_root.join(gitdir)
        };
        return resolve_git_common_dir_from(&gitdir_path);
    }
    anyhow::bail!("no .git at {}", repo_root.display())
}

/// Given a candidate gitdir (either a worktree's per-worktree dir or a
/// submodule's `.git/modules/<name>`), resolve to the common dir.
fn resolve_git_common_dir_from(gitdir: &Path) -> Result<PathBuf> {
    let gitdir_abs = gitdir
        .canonicalize()
        .unwrap_or_else(|_| gitdir.to_path_buf());
    let commondir_marker = gitdir_abs.join("commondir");
    if let Ok(rel) = std::fs::read_to_string(&commondir_marker) {
        let r = rel.trim();
        let joined = if Path::new(r).is_absolute() {
            PathBuf::from(r)
        } else {
            gitdir_abs.join(r)
        };
        return Ok(joined.canonicalize().unwrap_or(joined));
    }
    // No commondir marker → this gitdir IS the common dir (submodule case
    // or a directly-pointed-at gitdir).
    Ok(gitdir_abs)
}

/// Enumerate worktree paths by reading `.git/worktrees/*/gitdir`. Main
/// worktree is identified by `<git_common>/..`. Avoids spawning git.
fn list_worktrees(repo_root: &Path, git_common: &Path) -> Vec<PathBuf> {
    let mut out = vec![];
    let main = git_common
        .parent()
        .map(|p| p.to_path_buf())
        .unwrap_or_else(|| repo_root.to_path_buf());
    if let Ok(canon) = main.canonicalize() {
        out.push(canon);
    } else {
        out.push(main);
    }
    let wt_dir = git_common.join("worktrees");
    if let Ok(rd) = std::fs::read_dir(&wt_dir) {
        for ent in rd.flatten() {
            let gitdir_marker = ent.path().join("gitdir");
            let Ok(raw) = std::fs::read_to_string(&gitdir_marker) else {
                continue;
            };
            let p = PathBuf::from(raw.trim());
            // `gitdir` points at `<wt>/.git`; the worktree itself is its
            // parent.
            let wt = p.parent().map(|p| p.to_path_buf()).unwrap_or(p);
            let canon = wt.canonicalize().unwrap_or(wt);
            out.push(canon);
        }
    }
    out.sort();
    out.dedup();
    out
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
            hash_mode,
            on_modify,
            engine_hint: None,
        }
    }
    fn hs(pairs: &[(&str, &str)], mode: HashMode) -> MigrationHashSet {
        let mut by_key = std::collections::BTreeMap::new();
        for (k, v) in pairs {
            by_key.insert(k.to_string(), v.to_string());
        }
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

    fn tmp_dir(tag: &str) -> PathBuf {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let p = std::env::temp_dir().join(format!(
            "treeman-wt-test-{}-{}-{}",
            tag,
            std::process::id(),
            nanos
        ));
        std::fs::create_dir_all(&p).unwrap();
        p
    }

    fn run_git(cwd: &Path, args: &[&str]) {
        let out = std::process::Command::new("git")
            .arg("-C")
            .arg(cwd)
            .args(args)
            .output()
            .expect("git exec");
        assert!(
            out.status.success(),
            "git {args:?} failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn worktree_index_emits_add_and_remove() {
        let main = tmp_dir("main");
        run_git(&main, &["init", "-q", "-b", "main"]);
        run_git(&main, &["config", "user.email", "t@t"]);
        run_git(&main, &["config", "user.name", "t"]);
        run_git(&main, &["commit", "--allow-empty", "-q", "-m", "init"]);

        let (tx, mut rx) = mpsc::channel(8);
        let _h = spawn_worktree_index(main.clone(), 100, tx)
            .await
            .expect("spawn index");

        // Initial snapshot: contains main.
        let first = tokio::time::timeout(scaled(2000), rx.recv())
            .await
            .expect("initial diff timely")
            .expect("initial diff present");
        assert_eq!(first.removed, Vec::<PathBuf>::new());
        assert!(
            first
                .added
                .iter()
                .any(|p| p.canonicalize().unwrap_or_default()
                    == main.canonicalize().unwrap_or_default()),
            "initial added should include main, got {:?}",
            first.added
        );

        let wt_path = main.join(".worktrees").join("feature").join("foo");
        std::fs::create_dir_all(wt_path.parent().unwrap()).unwrap();
        run_git(
            &main,
            &[
                "worktree",
                "add",
                wt_path.to_str().unwrap(),
                "-b",
                "feature/foo",
            ],
        );

        let mut saw_added = false;
        let deadline = tokio::time::Instant::now() + scaled(5000);
        while tokio::time::Instant::now() < deadline && !saw_added {
            if let Ok(Some(diff)) = tokio::time::timeout(scaled(500), rx.recv()).await {
                if diff.added.iter().any(|p| p.ends_with("foo")) {
                    saw_added = true;
                }
            }
        }
        assert!(saw_added, "expected added event for feature/foo worktree");

        run_git(
            &main,
            &["worktree", "remove", "--force", wt_path.to_str().unwrap()],
        );

        let mut saw_removed = false;
        let deadline = tokio::time::Instant::now() + scaled(5000);
        while tokio::time::Instant::now() < deadline && !saw_removed {
            if let Ok(Some(diff)) = tokio::time::timeout(scaled(500), rx.recv()).await {
                if diff.removed.iter().any(|p| p.ends_with("foo")) {
                    saw_removed = true;
                }
            }
        }
        assert!(
            saw_removed,
            "expected removed event for feature/foo worktree"
        );

        let _ = std::fs::remove_dir_all(&main);
    }

    /// macOS uses FSEvents which coalesces over ~1s windows; Linux inotify
    /// is near-instant. Pad budgets generously on macOS so CI passes.
    #[cfg(target_os = "macos")]
    const TEST_SLOW_FACTOR: u32 = 4;
    #[cfg(not(target_os = "macos"))]
    const TEST_SLOW_FACTOR: u32 = 1;

    fn scaled(ms: u64) -> Duration {
        Duration::from_millis(ms * TEST_SLOW_FACTOR as u64)
    }

    /// Drain pending diffs after a fs operation, returning the union of all
    /// added/removed paths observed within `budget`. Idle window per recv is
    /// scaled with `TEST_SLOW_FACTOR` so macOS FSEvents has time to fire.
    async fn drain_diffs(
        rx: &mut mpsc::Receiver<WorktreeIndexDiff>,
        budget: Duration,
    ) -> (Vec<PathBuf>, Vec<PathBuf>) {
        let mut added = vec![];
        let mut removed = vec![];
        let deadline = tokio::time::Instant::now() + budget;
        let idle = scaled(800);
        while tokio::time::Instant::now() < deadline {
            match tokio::time::timeout(idle, rx.recv()).await {
                Ok(Some(diff)) => {
                    added.extend(diff.added);
                    removed.extend(diff.removed);
                }
                _ => break,
            }
        }
        (added, removed)
    }

    fn init_repo(tag: &str) -> PathBuf {
        let p = tmp_dir(tag);
        run_git(&p, &["init", "-q", "-b", "main"]);
        run_git(&p, &["config", "user.email", "t@t"]);
        run_git(&p, &["config", "user.name", "t"]);
        run_git(&p, &["commit", "--allow-empty", "-q", "-m", "init"]);
        p
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn worktree_move_emits_remove_then_add() {
        let main = init_repo("move");
        let (tx, mut rx) = mpsc::channel(16);
        let _h = spawn_worktree_index(main.clone(), 100, tx).await.unwrap();
        let _initial = tokio::time::timeout(scaled(2000), rx.recv())
            .await
            .unwrap()
            .unwrap();

        let from = main.join(".worktrees").join("feature").join("bar");
        std::fs::create_dir_all(from.parent().unwrap()).unwrap();
        run_git(
            &main,
            &[
                "worktree",
                "add",
                from.to_str().unwrap(),
                "-b",
                "feature/bar",
            ],
        );
        let (added, _) = drain_diffs(&mut rx, scaled(3000)).await;
        assert!(
            added.iter().any(|p| p.ends_with("bar")),
            "added bar: {added:?}"
        );

        let to = main.join(".worktrees").join("feature").join("baz");
        run_git(
            &main,
            &[
                "worktree",
                "move",
                from.to_str().unwrap(),
                to.to_str().unwrap(),
            ],
        );
        let (added2, removed2) = drain_diffs(&mut rx, scaled(3000)).await;
        assert!(
            removed2.iter().any(|p| p.ends_with("bar")),
            "removed bar: {removed2:?}"
        );
        assert!(
            added2.iter().any(|p| p.ends_with("baz")),
            "added baz: {added2:?}"
        );

        let _ = std::fs::remove_dir_all(&main);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn worktree_prune_emits_remove() {
        let main = init_repo("prune");
        let (tx, mut rx) = mpsc::channel(16);
        let _h = spawn_worktree_index(main.clone(), 100, tx).await.unwrap();
        let _initial = tokio::time::timeout(scaled(2000), rx.recv())
            .await
            .unwrap()
            .unwrap();

        let wt = main.join(".worktrees").join("stale");
        run_git(
            &main,
            &["worktree", "add", wt.to_str().unwrap(), "-b", "stale"],
        );
        let (added, _) = drain_diffs(&mut rx, scaled(3000)).await;
        assert!(
            added.iter().any(|p| p.ends_with("stale")),
            "added: {added:?}"
        );

        // Delete the worktree dir on disk without telling git → makes it
        // prunable.
        std::fs::remove_dir_all(&wt).unwrap();
        run_git(&main, &["worktree", "prune"]);

        let (_, removed) = drain_diffs(&mut rx, scaled(3000)).await;
        assert!(
            removed.iter().any(|p| p.ends_with("stale")),
            "removed: {removed:?}"
        );

        let _ = std::fs::remove_dir_all(&main);
    }

    #[test]
    fn common_dir_handles_symlinked_dot_git() {
        let d = tmp_dir("symlink");
        let real_git = d.join("real.git");
        std::fs::create_dir_all(real_git.join("worktrees")).unwrap();
        let link = d.join(".git");
        std::os::unix::fs::symlink(&real_git, &link).unwrap();
        let resolved = resolve_git_common_dir(&d).expect("resolve");
        assert_eq!(
            resolved,
            real_git.canonicalize().unwrap(),
            "symlinked .git should resolve to real path"
        );
        let _ = std::fs::remove_dir_all(&d);
    }

    #[test]
    fn common_dir_handles_submodule_style_gitfile() {
        // Submodule .git is a file pointing at `<super>/.git/modules/<name>/`
        // and has NO `commondir` marker — that gitdir IS the common dir.
        let sup = tmp_dir("submod");
        let module = sup.join(".git").join("modules").join("sub");
        std::fs::create_dir_all(module.join("worktrees")).unwrap();
        let sub = sup.join("sub");
        std::fs::create_dir_all(&sub).unwrap();
        std::fs::write(sub.join(".git"), format!("gitdir: {}\n", module.display())).unwrap();
        let resolved = resolve_git_common_dir(&sub).expect("resolve");
        assert_eq!(
            resolved,
            module.canonicalize().unwrap(),
            "submodule .git file should resolve to module dir"
        );
        let _ = std::fs::remove_dir_all(&sup);
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn worktree_rapid_churn_converges() {
        let main = init_repo("churn");
        let (tx, mut rx) = mpsc::channel(32);
        let _h = spawn_worktree_index(main.clone(), 100, tx).await.unwrap();
        let _initial = tokio::time::timeout(scaled(2000), rx.recv())
            .await
            .unwrap()
            .unwrap();

        let names = ["alpha", "beta", "gamma"];
        for n in &names {
            let wt = main.join(".worktrees").join(n);
            run_git(&main, &["worktree", "add", wt.to_str().unwrap(), "-b", n]);
        }
        let (added, _) = drain_diffs(&mut rx, scaled(3000)).await;
        for n in &names {
            assert!(added.iter().any(|p| p.ends_with(n)), "added {n}: {added:?}");
        }

        // Settle before second burst so the debounce window doesn't collapse
        // both into a net-zero diff.
        // macOS FSEvents coalesce more aggressively than inotify; give the
        // first burst plenty of time to settle before the second.
        tokio::time::sleep(scaled(1500)).await;

        for n in &names {
            let wt = main.join(".worktrees").join(n);
            run_git(
                &main,
                &["worktree", "remove", "--force", wt.to_str().unwrap()],
            );
        }
        let (_, removed) = drain_diffs(&mut rx, scaled(3000)).await;
        for n in &names {
            assert!(
                removed.iter().any(|p| p.ends_with(n)),
                "removed {n}: {removed:?}"
            );
        }

        let _ = std::fs::remove_dir_all(&main);
    }
}
