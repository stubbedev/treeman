//! Socket-path resolution. Actual unix-socket framing lands in M1 — for
//! now the daemon publishes an address sidecar (`<socket>.addr`) so the CLI
//! can dial a localhost JSON-RPC server.

use std::path::PathBuf;

use anyhow::{Context, Result};
use directories::ProjectDirs;
use treeman_proto::{SOCKET_BASENAME, SOCKET_ENV};

/// Resolution order: `$TREEMAN_SOCKET`, `$XDG_RUNTIME_DIR/treeman.sock`,
/// `~/.local/state/treeman/treeman.sock`.
pub fn resolve_path() -> Result<PathBuf> {
    if let Ok(s) = std::env::var(SOCKET_ENV) {
        return Ok(PathBuf::from(s));
    }
    if let Ok(rt) = std::env::var("XDG_RUNTIME_DIR") {
        if !rt.is_empty() {
            return Ok(PathBuf::from(rt).join(SOCKET_BASENAME));
        }
    }
    let dirs = ProjectDirs::from("", "", "treeman")
        .context("could not resolve treeman state directory")?;
    let state = dirs.data_local_dir();
    std::fs::create_dir_all(state)
        .with_context(|| format!("create {}", state.display()))?;
    Ok(state.join(SOCKET_BASENAME))
}

/// Remove a stale socket file (or its `.addr` sidecar) from a prior run.
pub fn clear_stale(socket_path: &std::path::Path) -> Result<()> {
    let _ = std::fs::remove_file(socket_path);
    let _ = std::fs::remove_file(socket_path.with_extension("addr"));
    Ok(())
}
