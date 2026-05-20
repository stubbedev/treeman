//! MySQL binlog delta replay. Used by the watcher's `Delta` dispatch
//! (added-only migration files) to apply the migration on the source DB
//! and stream the resulting binlog events into each paratest clone.
//!
//! Port of build_helper/internal/binlog + internal/delta. Uses
//! `mysql_async`'s binlog stream (the only mature pure-Rust impl as of
//! 2026; `mysql_binlog_connector_rust` is alpha).
//!
//! Replay strategy:
//!   1. Capture `SHOW MASTER STATUS` (file, pos) on the source.
//!   2. Caller runs `php artisan migrate` (or framework equivalent) on
//!      the source DB.
//!   3. Open a binlog stream from (file, pos) until the new master
//!      position is reached.
//!   4. For each Row event whose schema matches the source DB, rewrite
//!      the schema name to each target DB and emit a matching
//!      INSERT/UPDATE/DELETE.
//!
//! Requirements on the MySQL server:
//!   - `log_bin = ON`
//!   - `binlog_format = ROW`
//!   - `binlog_row_image = FULL`
//!   - replication user with REPLICATION SLAVE + RELOAD grants.

use anyhow::{Context, Result};
use futures_util::StreamExt;
use mysql_async::prelude::Queryable;
use mysql_async::{BinlogStreamRequest, Pool};
use tracing::{debug, info, warn};
use treeman_core::config::MysqlConn;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BinlogPosition {
    pub file: String,
    pub pos: u32,
}

pub struct BinlogReplicator {
    cfg: MysqlConn,
    pool: Pool,
    server_id: u32,
}

impl BinlogReplicator {
    pub async fn connect(cfg: &MysqlConn) -> Result<Self> {
        let password = cfg.password_env.as_deref()
            .map(|k| std::env::var(k).unwrap_or_default())
            .unwrap_or_default();
        let url = format!(
            "mysql://{user}:{pass}@{host}:{port}",
            user = cfg.user,
            pass = urlencode(&password),
            host = cfg.host, port = cfg.port,
        );
        let pool = Pool::from_url(&url).context("mysql_async pool")?;
        // Pseudo-random server_id that doesn't collide with the source.
        let server_id = 100_000 + (std::process::id() % 50_000);
        Ok(Self { cfg: cfg.clone(), pool, server_id })
    }

    /// `SHOW MASTER STATUS` → (file, pos). Returns None if binlog is off.
    pub async fn current_position(&self) -> Result<Option<BinlogPosition>> {
        let mut conn = self.pool.get_conn().await.context("acquire mysql conn")?;
        let row: Option<(String, u32)> = conn
            .query_first("SHOW MASTER STATUS")
            .await
            .context("SHOW MASTER STATUS")?;
        Ok(row.map(|(file, pos)| BinlogPosition { file, pos }))
    }

    /// Replay binlog events between `from` and current master position
    /// onto each target DB. Schema-name rewriting maps `source_db_name`
    /// → each entry in `target_dbs`.
    ///
    /// NOTE: This MVP iterates the binlog stream and counts events but
    /// does NOT yet execute the rewritten DML against targets — applying
    /// row-image events requires reconstructing INSERT/UPDATE/DELETE
    /// statements from the parsed row data, which is a substantial
    /// piece of work tracked under M9. For now this returns a summary
    /// of what would have been replayed so callers can wire the rest
    /// of the pipeline (watcher → snapshot → migrate → delta) and swap
    /// in the real applier later.
    pub async fn replay_range(
        &self,
        from: &BinlogPosition,
        source_db_name: &str,
        target_dbs: &[String],
    ) -> Result<ReplaySummary> {
        if target_dbs.is_empty() {
            return Ok(ReplaySummary::default());
        }
        let conn = self.pool.get_conn().await?;
        let request = BinlogStreamRequest::new(self.server_id)
            .with_filename(from.file.as_bytes())
            .with_pos(from.pos as u64);
        let mut stream = conn.get_binlog_stream(request).await
            .context("open binlog stream")?;

        let to = self.current_position().await?
            .context("no current master position (binlog off?)")?;
        debug!(?from, ?to, "replaying binlog range");

        let mut summary = ReplaySummary::default();
        while let Ok(Some(event)) = stream.next().await.transpose() {
            summary.events_seen += 1;
            let header = event.header();
            // Track the file position progress. mysql_async returns the
            // current position as `log_pos` on the event header.
            if header.log_pos() as u32 >= to.pos {
                // Reached the new master position.
                break;
            }
            // Real impl would inspect event.event_type() for ROW events
            // (WRITE_ROWS_EVENT, UPDATE_ROWS_EVENT, DELETE_ROWS_EVENT),
            // decode the table-map event preceding each ROW event to
            // map table_id → (schema, table, columns), match
            // schema == source_db_name, then for each row issue:
            //   INSERT INTO `<target>`.`<table>` VALUES (...)
            // …against each entry in `target_dbs`. Skipped here.
            summary.events_skipped += 1;
        }
        info!(
            events_seen = summary.events_seen,
            events_skipped = summary.events_skipped,
            targets = target_dbs.len(),
            source = source_db_name,
            "binlog replay summary (DML application is M9 stretch — counts only)"
        );
        warn!("binlog DML application is not yet implemented — see treeman-db::binlog M9 note");
        Ok(summary)
    }

    pub async fn close(self) -> Result<()> {
        self.pool.disconnect().await.context("pool disconnect")?;
        Ok(())
    }
}

#[derive(Debug, Default, Clone)]
pub struct ReplaySummary {
    pub events_seen: u64,
    pub events_skipped: u64,
}

fn urlencode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for c in s.chars() {
        if c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.' | '~') {
            out.push(c);
        } else {
            let mut buf = [0u8; 4];
            for b in c.encode_utf8(&mut buf).bytes() {
                out.push_str(&format!("%{:02X}", b));
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn position_equality() {
        let a = BinlogPosition { file: "bin.000001".into(), pos: 4 };
        let b = a.clone();
        assert_eq!(a, b);
    }

    #[test]
    fn summary_default() {
        let s = ReplaySummary::default();
        assert_eq!(s.events_seen, 0);
    }

    /// Suppress dead-code warning on the unused method when `binlog`
    /// feature in mysql_async is disabled.
    #[allow(dead_code)]
    fn _suppress() {
        let _ = |c: &BinlogReplicator| c.server_id;
    }
}

#[allow(dead_code)]
fn _suppress_field_unused(c: &BinlogReplicator) -> (&MysqlConn, u32) {
    (&c.cfg, c.server_id)
}
