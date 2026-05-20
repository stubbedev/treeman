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
//!   4. For each Row event whose schema matches the source DB,
//!      reconstruct INSERT/UPDATE/DELETE statements and re-execute them
//!      against each target DB (the paratest clones).
//!
//! Requirements on the MySQL server:
//!   - `log_bin = ON`
//!   - `binlog_format = ROW`
//!   - `binlog_row_image = FULL`
//!   - Replication user with REPLICATION SLAVE + RELOAD grants.
//!
//! Column names are resolved from `information_schema.COLUMNS` and
//! cached per `(schema, table)`. We do not use the TME's embedded
//! column-name metadata (`binlog_row_metadata=FULL`) because the
//! mysql_async 0.34 re-export's `iter_column_name` is lifetime-bound
//! to the TME's inner lifetime, which the binlog stream gives us as
//! `'static` — pulling that out of a non-`'static` reference doesn't
//! compile. An information_schema lookup per first-seen table is
//! cheap and works on every server that's running migrations.
//!
//! Unreachability: if the MySQL endpoint isn't reachable on TCP (e.g.
//! the engine runs in a container that does not expose the port to the
//! base OS), `connect` and `replay_range` return an explicit
//! `Unreachable` error — callers should treat it as "binlog path is
//! unavailable, fall back to delta-via-re-migrate or full rebuild".

use std::collections::HashMap;
use std::time::Duration;

use anyhow::{Context, Result, anyhow};
use futures_util::StreamExt;
use mysql_async::binlog::events::{EventData, RowsEventData};
use mysql_async::binlog::row::BinlogRow;
use mysql_async::prelude::Queryable;
use mysql_async::{BinlogStreamRequest, Params, Pool, Value};
use tracing::{debug, info, trace, warn};
use treeman_core::config::MysqlConn;

/// Regex for DDL detection. Anything starting with a write keyword
/// is a candidate for DDL replay against clones.
const DDL_KEYWORDS: &[&str] = &[
    "CREATE", "ALTER", "DROP", "RENAME", "TRUNCATE", "GRANT", "REVOKE",
];

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

#[derive(Debug, thiserror::Error)]
pub enum BinlogError {
    #[error(
        "mysql endpoint unreachable at {host}:{port} ({reason}) — binlog path unavailable, caller must fall back"
    )]
    Unreachable {
        host: String,
        port: u16,
        reason: String,
    },
    #[error("binlog is disabled on the server (SHOW MASTER STATUS returned no row)")]
    BinlogDisabled,
    #[error("{0}")]
    Other(#[from] anyhow::Error),
}

impl BinlogReplicator {
    /// Connect to the MySQL endpoint. Performs a fast TCP-level probe
    /// first so that an unreachable container surfaces as
    /// [`BinlogError::Unreachable`] instead of a generic connect error.
    pub async fn connect(cfg: &MysqlConn) -> std::result::Result<Self, BinlogError> {
        // TCP probe — 1.5s timeout. If the address can't be reached at
        // all we want a clear "fallback" signal rather than a sqlx-style
        // wrapped error.
        match tokio::time::timeout(
            Duration::from_millis(1500),
            tokio::net::TcpStream::connect((cfg.host.as_str(), cfg.port)),
        )
        .await
        {
            Err(_elapsed) => {
                return Err(BinlogError::Unreachable {
                    host: cfg.host.clone(),
                    port: cfg.port,
                    reason: "tcp connect timeout after 1500ms".into(),
                });
            }
            Ok(Err(e)) => {
                return Err(BinlogError::Unreachable {
                    host: cfg.host.clone(),
                    port: cfg.port,
                    reason: format!("tcp connect refused: {e}"),
                });
            }
            Ok(Ok(_stream)) => { /* reachable; drop probe socket */ }
        }

        let password = cfg
            .password
            .clone()
            .or_else(|| {
                cfg.password_env
                    .as_deref()
                    .and_then(|k| std::env::var(k).ok())
            })
            .unwrap_or_default();
        let url = format!(
            "mysql://{user}:{pass}@{host}:{port}",
            user = cfg.user,
            pass = urlencode(&password),
            host = cfg.host,
            port = cfg.port,
        );
        let pool = Pool::from_url(&url).map_err(|e| BinlogError::Other(anyhow!(e)))?;
        // Pseudo-random server_id that doesn't collide with the source.
        let server_id = 100_000 + (std::process::id() % 50_000);
        Ok(Self {
            cfg: cfg.clone(),
            pool,
            server_id,
        })
    }

    /// `SHOW MASTER STATUS` → (file, pos). Returns
    /// `Err(BinlogError::BinlogDisabled)` if binlog is off.
    pub async fn current_position(&self) -> std::result::Result<BinlogPosition, BinlogError> {
        let mut conn = self
            .pool
            .get_conn()
            .await
            .map_err(|e| BinlogError::Other(anyhow!(e)))?;
        let row: Option<(String, u32)> = conn
            .query_first("SHOW MASTER STATUS")
            .await
            .map_err(|e| BinlogError::Other(anyhow!(e)))?;
        row.map(|(file, pos)| BinlogPosition { file, pos })
            .ok_or(BinlogError::BinlogDisabled)
    }

    /// Replay binlog events between `from` and current master position,
    /// rewriting each Row event from `source_db_name` to each entry in
    /// `target_dbs` and executing the reconstructed DML on the target.
    pub async fn replay_range(
        &self,
        from: &BinlogPosition,
        source_db_name: &str,
        target_dbs: &[String],
    ) -> std::result::Result<ReplaySummary, BinlogError> {
        if target_dbs.is_empty() {
            return Ok(ReplaySummary::default());
        }
        let to = self.current_position().await?;
        debug!(?from, ?to, "replaying binlog range");

        let conn = self
            .pool
            .get_conn()
            .await
            .map_err(|e| BinlogError::Other(anyhow!(e)))?;
        let request = BinlogStreamRequest::new(self.server_id)
            .with_filename(from.file.as_bytes())
            .with_pos(from.pos as u64);
        let mut stream = conn
            .get_binlog_stream(request)
            .await
            .map_err(|e| BinlogError::Other(anyhow!("open binlog stream: {e}")))?;

        // Writer pool for emitting reconstructed DML. Use a dedicated
        // connection per target so we can scope `USE` safely.
        let mut writers: HashMap<String, mysql_async::Conn> = HashMap::new();
        for tgt in target_dbs {
            let c = self
                .pool
                .get_conn()
                .await
                .map_err(|e| BinlogError::Other(anyhow!("writer pool acquire: {e}")))?;
            writers.insert(tgt.clone(), c);
        }

        // Cache of (schema, table) -> column names. First filled from
        // TME's optional column-name metadata (binlog_row_metadata=FULL);
        // failing that, queried from information_schema.
        let mut colcache: HashMap<(String, String), Vec<String>> = HashMap::new();

        let mut summary = ReplaySummary::default();

        while let Ok(Some(event)) = stream.next().await.transpose() {
            summary.events_seen += 1;
            let header = event.header();
            let data = match event.read_data() {
                Ok(Some(d)) => d,
                Ok(None) => continue,
                Err(e) => {
                    warn!(error = %e, "binlog event read_data error — skipping event");
                    summary.events_skipped += 1;
                    continue;
                }
            };

            // DDL — fan out via `USE <target>; <query>` to each clone.
            // Skip transaction control frames; they're noise without
            // begin/commit semantics across clones (we apply each
            // event in its own implicit txn).
            if let EventData::QueryEvent(qe) = data {
                let schema = qe.schema().into_owned();
                let q = qe.query().into_owned();
                let trimmed = q.trim();
                let upper = trimmed.to_uppercase();
                if upper == "BEGIN" || upper == "COMMIT" || upper == "ROLLBACK" {
                    continue;
                }
                // Only forward DDL whose schema matches the source.
                // Empty schema = server-level statements (CREATE USER,
                // etc.) — skip; they're not per-DB.
                if schema != source_db_name {
                    summary.events_other_schema += 1;
                    continue;
                }
                if !is_ddl(&upper) {
                    // Could be `DELETE FROM …` in MIXED format. We
                    // can't safely re-execute non-DDL statements
                    // against clones (they may already have the row
                    // events replayed). Skip and rely on row events
                    // for DML.
                    summary.events_skipped += 1;
                    continue;
                }
                summary.ddl_events_applied += 1;
                for target_db in target_dbs {
                    let conn = writers
                        .get_mut(target_db)
                        .expect("writer for target prepared above");
                    apply_ddl(conn, target_db, &q).await.with_context(|| {
                        format!(
                            "apply DDL on {target_db} (rewritten from schema {schema}): {}",
                            truncate_for_log(&q, 200)
                        )
                    })?;
                }

                if header.log_pos() >= to.pos {
                    debug!(pos = header.log_pos(), "reached target master position");
                    break;
                }
                continue;
            }

            if let EventData::RowsEvent(rows_event_data) = data {
                let table_id = rows_event_data.table_id();
                let tme = match stream.get_tme(table_id) {
                    Some(t) => t,
                    None => {
                        warn!(table_id, "no TME for table_id — skipping");
                        summary.events_skipped += 1;
                        continue;
                    }
                };

                let db = tme.database_name().into_owned();
                let table = tme.table_name().into_owned();
                if db != source_db_name {
                    summary.events_other_schema += 1;
                    continue;
                }

                let kind = row_kind(&rows_event_data);
                trace!(?kind, %db, %table, "replay row event");

                let col_names = resolve_columns(&mut colcache, &mut writers, &db, &table).await?;

                let row_iter = rows_event_data.rows(tme);
                for pair in row_iter {
                    let pair = pair
                        .map_err(|e| BinlogError::Other(anyhow!("rows iter decode error: {e}")))?;
                    for target_db in target_dbs {
                        let conn = writers
                            .get_mut(target_db)
                            .expect("writer for target prepared above");
                        apply_one(conn, target_db, &table, &col_names, &kind, &pair)
                            .await
                            .with_context(|| {
                                format!(
                                    "apply {:?} on {target_db}.{table} (rewrite from {db})",
                                    kind
                                )
                            })?;
                    }
                    summary.rows_applied += 1;
                }
                summary.row_events_applied += 1;
            }

            if header.log_pos() >= to.pos {
                debug!(pos = header.log_pos(), "reached target master position");
                break;
            }
        }

        for (_, c) in writers {
            // Best-effort disconnect; if it fails we don't care — pool
            // will reap on close.
            let _ = c.disconnect().await;
        }

        info!(
            events_seen = summary.events_seen,
            ddl_events_applied = summary.ddl_events_applied,
            row_events_applied = summary.row_events_applied,
            rows_applied = summary.rows_applied,
            events_other_schema = summary.events_other_schema,
            events_skipped = summary.events_skipped,
            targets = target_dbs.len(),
            source = source_db_name,
            "binlog replay complete"
        );
        Ok(summary)
    }

    pub async fn close(self) -> Result<()> {
        self.pool.disconnect().await.context("pool disconnect")?;
        Ok(())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RowKind {
    Insert,
    Update,
    Delete,
}

fn row_kind(re: &RowsEventData<'_>) -> RowKind {
    use RowsEventData::*;
    match re {
        WriteRowsEventV1(_) | WriteRowsEvent(_) => RowKind::Insert,
        UpdateRowsEventV1(_) | UpdateRowsEvent(_) | PartialUpdateRowsEvent(_) => RowKind::Update,
        DeleteRowsEventV1(_) | DeleteRowsEvent(_) => RowKind::Delete,
    }
}

/// Resolve column names for `(db, table)` via information_schema,
/// cached. See module docstring for why we don't use the TME shortcut.
async fn resolve_columns(
    cache: &mut HashMap<(String, String), Vec<String>>,
    writers: &mut HashMap<String, mysql_async::Conn>,
    db: &str,
    table: &str,
) -> std::result::Result<Vec<String>, BinlogError> {
    if let Some(v) = cache.get(&(db.to_string(), table.to_string())) {
        return Ok(v.clone());
    }
    let conn = writers
        .values_mut()
        .next()
        .ok_or_else(|| BinlogError::Other(anyhow!("no writer connection available")))?;
    let rows: Vec<(String,)> = conn
        .exec(
            "SELECT COLUMN_NAME FROM information_schema.COLUMNS \
             WHERE TABLE_SCHEMA=? AND TABLE_NAME=? \
             ORDER BY ORDINAL_POSITION",
            (db, table),
        )
        .await
        .map_err(|e| BinlogError::Other(anyhow!("information_schema lookup: {e}")))?;
    let names: Vec<String> = rows.into_iter().map(|(n,)| n).collect();
    if names.is_empty() {
        return Err(BinlogError::Other(anyhow!(
            "no columns found for {db}.{table} — table missing on source?"
        )));
    }
    cache.insert((db.to_string(), table.to_string()), names.clone());
    Ok(names)
}

/// Apply one decoded row pair to `target_db.table`.
async fn apply_one(
    conn: &mut mysql_async::Conn,
    target_db: &str,
    table: &str,
    col_names: &[String],
    kind: &RowKind,
    pair: &(Option<BinlogRow>, Option<BinlogRow>),
) -> Result<()> {
    validate_ident(target_db)?;
    validate_ident(table)?;
    for c in col_names {
        validate_ident(c)?;
    }

    let cols_csv = col_names
        .iter()
        .map(|c| format!("`{c}`"))
        .collect::<Vec<_>>()
        .join(",");

    match kind {
        RowKind::Insert => {
            let after = pair
                .1
                .as_ref()
                .ok_or_else(|| anyhow!("WRITE event missing after-image"))?;
            let values = row_to_values(after)?;
            if values.len() != col_names.len() {
                anyhow::bail!(
                    "value count {} != column count {} for INSERT on `{target_db}`.`{table}` — \
                     ensure binlog_row_image=FULL on the source",
                    values.len(),
                    col_names.len()
                );
            }
            let placeholders = vec!["?"; values.len()].join(",");
            let stmt =
                format!("INSERT INTO `{target_db}`.`{table}` ({cols_csv}) VALUES ({placeholders})");
            conn.exec_drop(stmt, Params::Positional(values)).await?;
        }
        RowKind::Update => {
            let before = pair
                .0
                .as_ref()
                .ok_or_else(|| anyhow!("UPDATE event missing before-image"))?;
            let after = pair
                .1
                .as_ref()
                .ok_or_else(|| anyhow!("UPDATE event missing after-image"))?;
            let before_vals = row_to_values(before)?;
            let after_vals = row_to_values(after)?;
            if before_vals.len() != col_names.len() || after_vals.len() != col_names.len() {
                anyhow::bail!(
                    "UPDATE column-count mismatch for `{target_db}`.`{table}` — \
                     binlog_row_image=FULL required"
                );
            }
            let set_clause = col_names
                .iter()
                .map(|c| format!("`{c}`=?"))
                .collect::<Vec<_>>()
                .join(",");
            let (where_clause, where_params) = build_where(col_names, &before_vals);
            let stmt = format!(
                "UPDATE `{target_db}`.`{table}` SET {set_clause} WHERE {where_clause} LIMIT 1"
            );
            let mut params = after_vals;
            params.extend(where_params);
            conn.exec_drop(stmt, Params::Positional(params)).await?;
        }
        RowKind::Delete => {
            let before = pair
                .0
                .as_ref()
                .ok_or_else(|| anyhow!("DELETE event missing before-image"))?;
            let before_vals = row_to_values(before)?;
            if before_vals.len() != col_names.len() {
                anyhow::bail!(
                    "DELETE column-count mismatch for `{target_db}`.`{table}` — \
                     binlog_row_image=FULL required"
                );
            }
            let (where_clause, where_params) = build_where(col_names, &before_vals);
            let stmt = format!("DELETE FROM `{target_db}`.`{table}` WHERE {where_clause} LIMIT 1");
            conn.exec_drop(stmt, Params::Positional(where_params))
                .await?;
        }
    }
    Ok(())
}

/// Apply one DDL statement to `target_db`. We scope it via `USE`
/// before exec so unqualified table names in the statement bind to
/// the clone's namespace. Bare `db`. references in the original
/// statement would break — we DO NOT attempt to rewrite them because
/// well-formed migration DDL never names the source schema
/// explicitly (frameworks emit `CREATE TABLE \`t\`` not
/// `CREATE TABLE \`source_db\`.\`t\``). If a migration DOES qualify
/// against the source schema, this replay will fail loudly and the
/// caller falls back to per-clone framework migrate.
async fn apply_ddl(conn: &mut mysql_async::Conn, target_db: &str, query: &str) -> Result<()> {
    validate_ident(target_db)?;
    conn.query_drop(format!("USE `{target_db}`")).await?;
    conn.query_drop(query).await?;
    Ok(())
}

fn is_ddl(query_upper: &str) -> bool {
    DDL_KEYWORDS
        .iter()
        .any(|k| query_upper.starts_with(k) || query_upper.trim_start().starts_with(k))
}

fn truncate_for_log(s: &str, max: usize) -> String {
    if s.len() <= max {
        s.to_string()
    } else {
        format!("{}…", &s[..max])
    }
}

/// Build `col1 <eq?> ? AND col2 <eq?> ? ...` using `IS NULL` for NULL
/// values (since `col = NULL` is never true in SQL).
fn build_where(col_names: &[String], values: &[Value]) -> (String, Vec<Value>) {
    let mut parts: Vec<String> = Vec::with_capacity(col_names.len());
    let mut params: Vec<Value> = Vec::with_capacity(col_names.len());
    for (name, v) in col_names.iter().zip(values.iter()) {
        match v {
            Value::NULL => parts.push(format!("`{name}` IS NULL")),
            _ => {
                parts.push(format!("`{name}`=?"));
                params.push(v.clone());
            }
        }
    }
    (parts.join(" AND "), params)
}

fn row_to_values(row: &BinlogRow) -> Result<Vec<Value>> {
    use mysql_async::binlog::value::BinlogValue;
    let mut out = Vec::with_capacity(row.len());
    for i in 0..row.len() {
        match row.as_ref(i) {
            None => out.push(Value::NULL),
            Some(bv) => {
                // Clone — BinlogValue is Clone via inner Cow data.
                let owned = match bv {
                    BinlogValue::Value(v) => BinlogValue::Value(v.clone()),
                    BinlogValue::Jsonb(j) => BinlogValue::Jsonb(j.clone()),
                    BinlogValue::JsonDiff(d) => BinlogValue::JsonDiff(d.clone()),
                };
                let v: Value = owned
                    .try_into()
                    .map_err(|e| anyhow!("binlog value -> value: {e:?}"))?;
                out.push(v);
            }
        }
    }
    Ok(out)
}

#[derive(Debug, Default, Clone)]
pub struct ReplaySummary {
    pub events_seen: u64,
    pub events_skipped: u64,
    pub events_other_schema: u64,
    pub row_events_applied: u64,
    pub rows_applied: u64,
    pub ddl_events_applied: u64,
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

fn validate_ident(s: &str) -> Result<()> {
    if s.is_empty() {
        anyhow::bail!("empty identifier");
    }
    for c in s.chars() {
        if !(c.is_ascii_alphanumeric() || c == '_') {
            anyhow::bail!("invalid mysql identifier: {s:?}");
        }
    }
    Ok(())
}

#[allow(dead_code)]
fn _suppress_field_unused(c: &BinlogReplicator) -> (&MysqlConn, u32) {
    (&c.cfg, c.server_id)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn position_equality() {
        let a = BinlogPosition {
            file: "bin.000001".into(),
            pos: 4,
        };
        let b = a.clone();
        assert_eq!(a, b);
    }

    #[test]
    fn summary_default() {
        let s = ReplaySummary::default();
        assert_eq!(s.events_seen, 0);
        assert_eq!(s.rows_applied, 0);
    }

    #[test]
    fn build_where_handles_null() {
        let cols = vec!["a".to_string(), "b".to_string(), "c".to_string()];
        let vals = vec![Value::Int(1), Value::NULL, Value::Bytes(b"x".to_vec())];
        let (w, p) = build_where(&cols, &vals);
        assert_eq!(w, "`a`=? AND `b` IS NULL AND `c`=?");
        assert_eq!(p.len(), 2);
    }

    #[test]
    fn validate_ident_rejects_bad() {
        assert!(validate_ident("foo").is_ok());
        assert!(validate_ident("foo_bar123").is_ok());
        assert!(validate_ident("foo`bar").is_err());
        assert!(validate_ident("foo;drop").is_err());
        assert!(validate_ident("").is_err());
    }

    #[test]
    fn urlencode_basic() {
        assert_eq!(urlencode("abc"), "abc");
        assert_eq!(urlencode("a b"), "a%20b");
        assert_eq!(urlencode("p@ss"), "p%40ss");
    }

    #[test]
    fn is_ddl_recognizes_ddl_keywords() {
        for q in [
            "CREATE TABLE foo (id int)",
            "ALTER TABLE foo ADD COLUMN bar VARCHAR(255)",
            "DROP TABLE foo",
            "RENAME TABLE foo TO bar",
            "TRUNCATE TABLE foo",
            "  CREATE INDEX ix_foo ON foo (id)",
        ] {
            let upper = q.to_uppercase();
            assert!(is_ddl(&upper), "should be DDL: {q}");
        }
    }

    #[test]
    fn is_ddl_rejects_dml() {
        for q in [
            "INSERT INTO foo VALUES (1)",
            "UPDATE foo SET bar=1",
            "DELETE FROM foo",
            "SELECT * FROM foo",
        ] {
            let upper = q.to_uppercase();
            assert!(!is_ddl(&upper), "should NOT be DDL: {q}");
        }
    }

    #[test]
    fn truncate_for_log_caps_long_strings() {
        let long = "a".repeat(500);
        assert_eq!(truncate_for_log(&long, 10), "aaaaaaaaaa…");
        let short = "short";
        assert_eq!(truncate_for_log(short, 10), "short");
    }
}
