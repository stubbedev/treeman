//! Thin unix-socket JSON-line client for talking to `treemand`.

use std::path::PathBuf;

use anyhow::{Context, Result, bail};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixStream;
use treeman_proto::{Request, Response, SOCKET_BASENAME, SOCKET_ENV};

pub fn socket_path() -> Result<PathBuf> {
    if let Ok(s) = std::env::var(SOCKET_ENV) {
        return Ok(PathBuf::from(s));
    }
    if let Ok(rt) = std::env::var("XDG_RUNTIME_DIR") {
        if !rt.is_empty() {
            return Ok(PathBuf::from(rt).join(SOCKET_BASENAME));
        }
    }
    let dirs = directories::ProjectDirs::from("", "", "treeman")
        .context("project dirs unavailable")?;
    Ok(dirs.data_local_dir().join(SOCKET_BASENAME))
}

pub async fn call(req: Request) -> Result<Response> {
    let path = socket_path()?;
    let stream = UnixStream::connect(&path).await
        .with_context(|| format!("connect {} — is treemand running?", path.display()))?;
    let (read, mut write) = stream.into_split();
    let mut reader = BufReader::new(read);
    let mut line = serde_json::to_string(&req)?;
    line.push('\n');
    write.write_all(line.as_bytes()).await?;
    let mut resp_line = String::new();
    let n = reader.read_line(&mut resp_line).await?;
    if n == 0 { bail!("daemon closed connection"); }
    let resp: Response = serde_json::from_str(resp_line.trim_end())?;
    Ok(resp)
}
