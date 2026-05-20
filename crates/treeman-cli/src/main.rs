//! `treeman` — thin CLI client for `treemand`. M0 implements `status` only.

use std::path::PathBuf;

use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand};
use directories::ProjectDirs;
use jsonrpsee::core::client::ClientT;
use jsonrpsee::http_client::HttpClientBuilder;
use jsonrpsee::rpc_params;
use treeman_proto::{SOCKET_BASENAME, SOCKET_ENV, StatusResponse};

#[derive(Parser, Debug)]
#[command(name = "treeman", version, about = "Treeman CLI", long_about = None)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand, Debug)]
enum Cmd {
    /// Print daemon version, pid, uptime.
    Status,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.cmd {
        Cmd::Status => status().await,
    }
}

async fn status() -> Result<()> {
    let addr = resolve_addr()?;
    let url = format!("http://{addr}");
    let client = HttpClientBuilder::default().build(&url)
        .with_context(|| format!("connect {url}"))?;
    let resp: StatusResponse = client
        .request("status", rpc_params![])
        .await
        .context("status rpc")?;
    println!("daemon_version: {}", resp.daemon_version);
    println!("protocol:       v{}", resp.protocol_version);
    println!("pid:            {}", resp.pid);
    println!("started_at:     {}", resp.started_at_unix);
    println!("watchers:       {}", resp.watcher_count);
    Ok(())
}

fn resolve_addr() -> Result<String> {
    let socket_path = resolve_socket_path()?;
    let addr_path = socket_path.with_extension("addr");
    let addr = std::fs::read_to_string(&addr_path).with_context(|| {
        format!(
            "could not read {} — is treemand running? (start with `treemand`)",
            addr_path.display()
        )
    })?;
    let trimmed = addr.trim().to_string();
    if trimmed.is_empty() {
        bail!("{} is empty", addr_path.display());
    }
    Ok(trimmed)
}

fn resolve_socket_path() -> Result<PathBuf> {
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
    Ok(dirs.data_local_dir().join(SOCKET_BASENAME))
}
