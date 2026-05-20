//! `treemand` — Treeman daemon. Listens on a unix socket and serves
//! JSON-RPC requests from the `treeman` CLI.

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
    let db_path = treeman_store::default_db_path()?;
    let pool = treeman_store::open(&db_path).await?;
    treeman_store::init_subscriber(pool.clone())?;

    info!(db = %db_path.display(), "treemand starting");

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

    let server = ServerBuilder::default()
        .build("127.0.0.1:0")
        .await?;
    let local_addr = server.local_addr()?;

    let addr_path = socket_path.with_extension("addr");
    tokio::fs::write(&addr_path, local_addr.to_string()).await?;
    info!(
        event_type = "daemon_started",
        addr = %local_addr,
        pid = pid as i64,
        "treemand listening"
    );

    let handle: ServerHandle = server.start(module);

    let mut sigint = signal(SignalKind::interrupt())?;
    let mut sigterm = signal(SignalKind::terminate())?;
    tokio::select! {
        _ = sigint.recv()  => info!(event_type = "daemon_stopped", "SIGINT received"),
        _ = sigterm.recv() => info!(event_type = "daemon_stopped", "SIGTERM received"),
    }

    if let Err(e) = tokio::fs::remove_file(&addr_path).await {
        warn!(error = %e, "could not remove addr file");
    }
    handle.stop()?;
    handle.stopped().await;
    // Let the writer drain.
    tokio::time::sleep(std::time::Duration::from_millis(150)).await;
    Ok(())
}

#[derive(Clone)]
struct DaemonState {
    started_at_unix: i64,
}
