//! `treemand` — Treeman daemon. Serves newline-delimited JSON RPC over
//! a unix domain socket. Owns per-repo watcher tasks + the shared
//! event/watcher state.

mod socket;
mod state;

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::{UnixListener, UnixStream};
use tokio::signal::unix::{SignalKind, signal};
use tokio::sync::Notify;
use tracing::{error, info, warn};
use treeman_proto::{PROTOCOL_VERSION, Request, Response, StatusResponse};

const DAEMON_VERSION: &str = env!("CARGO_PKG_VERSION");

#[tokio::main]
async fn main() -> Result<()> {
    let db_path = treeman_store::default_db_path()?;
    let pool = treeman_store::open(&db_path).await?;
    treeman_store::init_subscriber(pool.clone())?;
    info!(db = %db_path.display(), "treemand starting");

    let socket_path = socket::resolve_path()?;
    socket::clear_stale(&socket_path)?;
    if let Some(parent) = socket_path.parent() {
        std::fs::create_dir_all(parent).ok();
    }
    let listener = UnixListener::bind(&socket_path)
        .with_context(|| format!("bind unix socket {}", socket_path.display()))?;
    socket::lockdown(&socket_path)?;

    let started_at_unix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    let pid = std::process::id();
    let shutdown = Arc::new(Notify::new());
    let state = Arc::new(state::DaemonState::new(pool.clone(), started_at_unix, pid));

    info!(event_type = "daemon_started", socket = %socket_path.display(),
          pid = pid as i64, "treemand listening");

    // Periodic snapshot GC. Walks the catalog per
    // `cfg.snapshots.retention.*` policy and drops evicted templates from
    // their engines. Interval is configured per-repo via
    // `snapshots.retention.gc_interval_minutes` (default 60); we use the
    // smallest interval across all registered repos so each gets at least
    // its own cadence.
    let gc_pool = pool.clone();
    let gc_shutdown = Arc::clone(&shutdown);
    tokio::spawn(async move {
        // Short startup delay so the first GC doesn't fight watcher resume.
        tokio::time::sleep(std::time::Duration::from_secs(30)).await;
        loop {
            let interval_minutes = resolve_gc_interval(&gc_pool).await;
            let cfg = match resolve_gc_cfg(&gc_pool).await {
                Some(c) => c,
                None => {
                    // No repos registered → nothing to GC. Re-poll on
                    // interval in case one gets registered.
                    tokio::select! {
                        _ = tokio::time::sleep(std::time::Duration::from_secs(
                            (interval_minutes * 60).into()
                        )) => {},
                        _ = gc_shutdown.notified() => break,
                    }
                    continue;
                }
            };
            match treeman_snapshot::run_gc(&gc_pool, &cfg).await {
                Ok(report) if report.catalog_evicted > 0 => {
                    info!(
                        evicted = report.catalog_evicted,
                        dropped = report.engine_dropped,
                        failed = report.engine_failed,
                        "snapshot GC pass complete"
                    );
                }
                Ok(_) => {}
                Err(e) => warn!(error = %e, "snapshot GC pass failed"),
            }
            tokio::select! {
                _ = tokio::time::sleep(std::time::Duration::from_secs(
                    (interval_minutes * 60).into()
                )) => {},
                _ = gc_shutdown.notified() => break,
            }
        }
    });

    // Auto-resume watchers for every repo registered in SQLite. Lets
    // `systemctl restart treemand` regain coverage without manual
    // `treeman watcher start` calls.
    match treeman_store::list_repo_paths(state.sqlite()).await {
        Ok(paths) => {
            for p in paths {
                match state.start_watcher(&p).await {
                    Ok(()) => info!(repo = %p, "resumed watcher"),
                    Err(e) => warn!(repo = %p, error = %e, "resume watcher failed"),
                }
            }
        }
        Err(e) => warn!(error = %e, "list_repo_paths failed"),
    }

    let mut sigint = signal(SignalKind::interrupt())?;
    let mut sigterm = signal(SignalKind::terminate())?;
    loop {
        tokio::select! {
            _ = sigint.recv()  => { info!(event_type = "daemon_stopped", "SIGINT received"); break; }
            _ = sigterm.recv() => { info!(event_type = "daemon_stopped", "SIGTERM received"); break; }
            _ = shutdown.notified() => { info!(event_type = "daemon_stopped", "shutdown requested"); break; }
            accept = listener.accept() => match accept {
                Ok((stream, _addr)) => {
                    if let Err(e) = socket::check_peer_uid(&stream) {
                        warn!(error = %e, "rejecting peer");
                        continue;
                    }
                    let state = Arc::clone(&state);
                    let shutdown = Arc::clone(&shutdown);
                    tokio::spawn(handle_conn(stream, state, shutdown));
                }
                Err(e) => error!(error = %e, "accept failed"),
            },
        }
    }
    std::fs::remove_file(&socket_path).ok();
    // Allow the SQLite layer to drain.
    tokio::time::sleep(std::time::Duration::from_millis(150)).await;
    Ok(())
}

async fn handle_conn(stream: UnixStream, state: Arc<state::DaemonState>, shutdown: Arc<Notify>) {
    let (read_half, mut write_half) = stream.into_split();
    let mut reader = BufReader::new(read_half);
    let mut line = String::new();
    loop {
        line.clear();
        let n = match reader.read_line(&mut line).await {
            Ok(n) => n,
            Err(_) => break,
        };
        if n == 0 {
            break;
        }
        let req: Request = match serde_json::from_str(line.trim_end()) {
            Ok(r) => r,
            Err(e) => {
                let _ = write_response(
                    &mut write_half,
                    &Response::Error {
                        message: format!("parse: {e}"),
                    },
                )
                .await;
                continue;
            }
        };
        let resp = dispatch(req, &state, &shutdown).await;
        if let Err(e) = write_response(&mut write_half, &resp).await {
            warn!(error = %e, "write response");
            break;
        }
    }
}

async fn dispatch(
    req: Request,
    state: &Arc<state::DaemonState>,
    shutdown: &Arc<Notify>,
) -> Response {
    match req {
        Request::Status => Response::Status(StatusResponse {
            protocol_version: PROTOCOL_VERSION,
            daemon_version: DAEMON_VERSION.to_string(),
            pid: state.pid,
            started_at_unix: state.started_at_unix,
            watcher_count: state.watcher_count() as u32,
        }),
        Request::Ping => Response::Pong,
        Request::RepoRegister { path, name } => {
            match treeman_store::ensure_repo(state.sqlite(), std::path::Path::new(&path), &name)
                .await
            {
                Ok(repo_id) => Response::RepoRegistered { repo_id },
                Err(e) => Response::Error {
                    message: e.to_string(),
                },
            }
        }
        Request::WatcherStart { repo_path } => match state.start_watcher(&repo_path).await {
            Ok(_) => Response::WatcherStarted { repo_path },
            Err(e) => Response::Error {
                message: e.to_string(),
            },
        },
        Request::WatcherStop { repo_path } => match state.stop_watcher(&repo_path).await {
            Ok(_) => Response::WatcherStopped { repo_path },
            Err(e) => Response::Error {
                message: e.to_string(),
            },
        },
        Request::WatcherList => Response::WatcherList {
            repos: state.list_watchers(),
        },
        Request::WorktreeList { repo_path } => match state.list_worktrees(&repo_path) {
            Ok(worktrees) => Response::WorktreeList { worktrees },
            Err(e) => Response::Error {
                message: e.to_string(),
            },
        },
        Request::WorktreeFinalize {
            repo_path,
            worktree_path,
        } => {
            let st = state.clone();
            let repo_for_task = repo_path.clone();
            let wt_for_task = worktree_path.clone();
            tokio::spawn(async move {
                if let Err(e) = finalize_worktree(&st, &repo_for_task, &wt_for_task).await {
                    let _ = treeman_store::write_event(
                        st.sqlite(),
                        "error",
                        "wt_finalize",
                        Some(&e.to_string()),
                        None,
                        None,
                        None,
                        None,
                        &serde_json::json!({
                            "repo_path": repo_for_task,
                            "worktree_path": wt_for_task,
                        })
                        .to_string(),
                    )
                    .await;
                }
            });
            Response::WorktreeFinalizeQueued { worktree_path }
        }
        Request::Shutdown => {
            shutdown.notify_one();
            Response::Ok
        }
    }
}

/// Background tail of `treeman wt create` when
/// `worktrees.async_create` is enabled: postcreate hooks + prepare
/// (DB ensure + dump-load + framework migrate + paratest clones).
/// All output is mirrored into the SQLite event log via the
/// daemon's tracing subscriber — `treeman logs tail -f` follows it.
async fn finalize_worktree(
    state: &Arc<state::DaemonState>,
    repo_path: &str,
    worktree_path: &str,
) -> anyhow::Result<()> {
    let repo_root = std::path::PathBuf::from(repo_path);
    let wt_root = std::path::PathBuf::from(worktree_path);
    let cfg = treeman_core::config::load_layered_for_worktree(&repo_root, &wt_root)?;
    let slug = treeman_core::slug_for(&wt_root, None);

    let repo_name = repo_root
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("repo");
    let repo_id = treeman_store::ensure_repo(state.sqlite(), &repo_root, repo_name).await?;
    let wt_id =
        treeman_store::ensure_worktree(state.sqlite(), repo_id, &wt_root, &slug.value, None)
            .await?;

    let _ = treeman_store::write_event(
        state.sqlite(),
        "info",
        "wt_finalize_start",
        Some("daemon-detached postcreate + prepare beginning"),
        Some(repo_id),
        Some(wt_id),
        None,
        None,
        "{}",
    )
    .await;

    if !cfg.hooks.postcreate.is_empty() {
        let outcome = treeman_core::hooks::run_hooks(
            &cfg.hooks.postcreate,
            &repo_root,
            &wt_root,
            &slug.value,
        )
        .await?;
        if outcome.aggregate_exit_code != 0 {
            anyhow::bail!(
                "postcreate hooks exited non-zero ({})",
                outcome.aggregate_exit_code
            );
        }
    }

    if !cfg.databases.is_empty() {
        treeman_prepare::run(&cfg, &repo_root, &slug, state.sqlite(), repo_id, wt_id).await?;
    }

    let _ = treeman_store::write_event(
        state.sqlite(),
        "info",
        "wt_finalize_done",
        Some("daemon-detached postcreate + prepare complete"),
        Some(repo_id),
        Some(wt_id),
        None,
        None,
        "{}",
    )
    .await;
    Ok(())
}

/// Pick the GC interval. Strategy: use the smallest
/// `snapshots.retention.gc_interval_minutes` across all registered repos,
/// floor-clamped to 5 minutes. If no repos are registered or no configs
/// parse, fall back to the schema default (60 min).
async fn resolve_gc_interval(pool: &sqlx::SqlitePool) -> u32 {
    let default = 60u32;
    let Ok(paths) = treeman_store::list_repo_paths(pool).await else {
        return default;
    };
    if paths.is_empty() {
        return default;
    }
    let mut min = u32::MAX;
    for p in &paths {
        let root = std::path::PathBuf::from(p);
        if let Ok(cfg) = treeman_core::config::load_layered(Some(&root)) {
            let v = cfg.snapshots.retention.gc_interval_minutes;
            if v > 0 && v < min {
                min = v;
            }
        }
    }
    if min == u32::MAX { default } else { min.max(5) }
}

/// Pick a single `Config` for GC. We need engine connection info; if
/// multiple repos are registered we just take the first one that loads —
/// the connections block is global per-host in practice (same MySQL/Pg
/// server hosts every repo's templates).
async fn resolve_gc_cfg(pool: &sqlx::SqlitePool) -> Option<treeman_core::config::Config> {
    let paths = treeman_store::list_repo_paths(pool).await.ok()?;
    for p in &paths {
        let root = std::path::PathBuf::from(p);
        if let Ok(cfg) = treeman_core::config::load_layered(Some(&root)) {
            return Some(cfg);
        }
    }
    None
}

async fn write_response(
    write: &mut tokio::net::unix::OwnedWriteHalf,
    resp: &Response,
) -> Result<()> {
    let mut s = serde_json::to_string(resp)?;
    s.push('\n');
    write.write_all(s.as_bytes()).await?;
    Ok(())
}
