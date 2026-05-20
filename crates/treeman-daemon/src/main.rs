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
        Request::Shutdown => {
            shutdown.notify_one();
            Response::Ok
        }
    }
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
