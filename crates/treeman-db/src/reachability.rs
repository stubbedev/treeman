//! TCP reachability probe used by the engine drivers' `connect()` so
//! that an unreachable container (e.g. dockerised MySQL whose port is
//! not exposed to the base OS) surfaces as a clear error before sqlx /
//! mongodb / redis / reqwest wraps it in driver-specific noise.
//!
//! The probe is best-effort: a successful TCP connect just proves the
//! port is reachable, not that the service is healthy. Drivers still
//! complete their normal auth handshake; the probe just guarantees
//! that, when the host:port is unreachable at all, callers see "X
//! unreachable at host:port" instead of e.g. a sqlx connect timeout
//! deep inside the pool. See [`crate::binlog::BinlogError::Unreachable`]
//! for the same shape applied to the binlog stream.

use std::time::Duration;

use anyhow::{Result, anyhow};
use url::Url;

/// Default probe timeout. Chosen short because a healthy local host
/// connects in <10ms, and the worst real-world case (a stale Docker
/// network not exposing the port) fails immediately with ECONNREFUSED.
pub const DEFAULT_TIMEOUT_MS: u64 = 1500;

/// Probe a `host:port` for TCP reachability. On failure returns an
/// anyhow error whose message includes `engine` so callers don't have
/// to wrap it.
pub async fn probe(engine: &str, host: &str, port: u16) -> Result<()> {
    probe_with_timeout(
        engine,
        host,
        port,
        Duration::from_millis(DEFAULT_TIMEOUT_MS),
    )
    .await
}

pub async fn probe_with_timeout(
    engine: &str,
    host: &str,
    port: u16,
    timeout: Duration,
) -> Result<()> {
    match tokio::time::timeout(timeout, tokio::net::TcpStream::connect((host, port))).await {
        Err(_elapsed) => Err(anyhow!(
            "{engine} unreachable at {host}:{port} (tcp connect timeout after {}ms — \
             is the service running and the port exposed to this host?)",
            timeout.as_millis()
        )),
        Ok(Err(e)) => Err(anyhow!(
            "{engine} unreachable at {host}:{port} (tcp connect refused: {e} — \
             check container port mapping or service binding)"
        )),
        Ok(Ok(_)) => Ok(()),
    }
}

/// Probe a URL-style connection string. Used by mongo / redis / es /
/// any HTTP engine. Falls back to the default port for the scheme
/// when the URL omits one.
pub async fn probe_url(engine: &str, raw: &str) -> Result<()> {
    let (host, port) = parse_host_port(engine, raw)?;
    probe(engine, &host, port).await
}

fn parse_host_port(engine: &str, raw: &str) -> Result<(String, u16)> {
    let url = Url::parse(raw).map_err(|e| anyhow!("{engine}: invalid URI {raw:?}: {e}"))?;
    let host = url
        .host_str()
        .ok_or_else(|| anyhow!("{engine}: no host in URI {raw:?}"))?
        .to_string();
    let port = url
        .port()
        .or_else(|| default_port_for_scheme(url.scheme()))
        .ok_or_else(|| anyhow!("{engine}: no port in URI {raw:?} and no default for scheme"))?;
    Ok((host, port))
}

fn default_port_for_scheme(scheme: &str) -> Option<u16> {
    Some(match scheme {
        "http" => 80,
        "https" => 443,
        "redis" => 6379,
        "rediss" => 6379,
        "mongodb" | "mongodb+srv" => 27017,
        "mysql" => 3306,
        "postgres" | "postgresql" => 5432,
        _ => return None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn probe_unreachable_port() {
        // 1 is reserved; almost certainly nothing listening locally.
        let err = probe_with_timeout("test", "127.0.0.1", 1, Duration::from_millis(200))
            .await
            .unwrap_err()
            .to_string();
        assert!(err.contains("test"));
        assert!(err.contains("127.0.0.1:1"));
    }

    #[test]
    fn parse_redis_default_port() {
        let (h, p) = parse_host_port("redis", "redis://localhost").unwrap();
        assert_eq!(h, "localhost");
        assert_eq!(p, 6379);
    }

    #[test]
    fn parse_mongo_with_explicit_port() {
        let (h, p) = parse_host_port("mongo", "mongodb://10.0.0.5:27018").unwrap();
        assert_eq!(h, "10.0.0.5");
        assert_eq!(p, 27018);
    }

    #[test]
    fn parse_http_default_port() {
        let (_, p) = parse_host_port("es", "http://localhost").unwrap();
        assert_eq!(p, 80);
        let (_, p) = parse_host_port("es", "https://localhost").unwrap();
        assert_eq!(p, 443);
    }
}
