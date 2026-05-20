//! `treemand` — Treeman daemon. Listens on a unix socket and serves
//! JSON-RPC requests from the `treeman` CLI.
//!
//! M0 only implements `status`. Subsequent milestones layer in
//! `repo.register`, `worktree.register`, `hook.run`, `snapshot.*`, `logs.*`.

mod socket;

use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::Result;
use jsonrpsee::RpcModule;
use jsonrpsee::server::{ServerBuilder, ServerHandle};
use tokio::signal::unix::{SignalKind, signal};
use tracing::{info, warn};
use treeman_proto::{PROTOCOL_VERSION, StatusResponse};

const DAEMON_VERSION: &str = env!("CARGO_PKG_VERSION");

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .with_target(false)
        .init();

    let socket_path = socket::resolve_path()?;
    socket::clear_stale(&socket_path)?;

    let started_at_unix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    let pid = std::process::id();

    let state = DaemonState { started_at_unix };

    let mut module = RpcModule::new(state);
    module.register_method("status", move |_params, state, _ext| {
        Ok::<_, jsonrpsee::types::ErrorObjectOwned>(StatusResponse {
            protocol_version: PROTOCOL_VERSION,
            daemon_version: DAEMON_VERSION.to_string(),
            pid,
            started_at_unix: state.started_at_unix,
            watcher_count: 0,
        })
    })?;

    // jsonrpsee 0.24 ships a stable HTTP/WS server but no first-class
    // unix-socket transport; we accept the unix socket ourselves and bridge
    // each connection via tower::Service. For M0 use the lightweight HTTP
    // wire on a unix socket — the protocol is JSON-RPC either way and the
    // CLI client only has to know how to send framed JSON.
    //
    // Approach: bind a localhost TCP server on an ephemeral port and have
    // the CLI dial that; replace with a unix-socket+JSON-line transport
    // in M1 once `socket::serve_jsonrpc` is implemented.
    let server = ServerBuilder::default()
        .build("127.0.0.1:0")
        .await?;
    let local_addr = server.local_addr()?;

    // Write the address to a sidecar file alongside the socket path so the
    // CLI can find it. M1 replaces this with a proper unix socket.
    let addr_path = socket_path.with_extension("addr");
    tokio::fs::write(&addr_path, local_addr.to_string()).await?;
    info!(addr = %local_addr, addr_path = %addr_path.display(), "treemand listening");

    let handle: ServerHandle = server.start(module);

    // Wait for SIGINT / SIGTERM, then shutdown cleanly.
    let mut sigint = signal(SignalKind::interrupt())?;
    let mut sigterm = signal(SignalKind::terminate())?;
    tokio::select! {
        _ = sigint.recv()  => info!("SIGINT, shutting down"),
        _ = sigterm.recv() => info!("SIGTERM, shutting down"),
    }

    if let Err(e) = tokio::fs::remove_file(&addr_path).await {
        warn!(error = %e, "could not remove addr file");
    }
    handle.stop()?;
    handle.stopped().await;
    Ok(())
}

#[derive(Clone)]
struct DaemonState {
    started_at_unix: i64,
}
