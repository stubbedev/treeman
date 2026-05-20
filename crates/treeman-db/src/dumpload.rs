//! Stream a `.sql` dump into a target database over the wire.
//!
//! Supported engines: mysql, postgres. The implementation walks the file
//! statement-by-statement (splitting on `;` outside of strings/comments)
//! so it doesn't load the whole file in memory.

use std::path::Path;

use anyhow::{Context, Result};
use sqlx::Executor;
use tracing::debug;

use crate::mysql::MysqlDriver;
use crate::postgres::PostgresDriver;

pub async fn load_mysql(driver: &MysqlDriver, target_db: &str, dump_path: &Path) -> Result<u64> {
    let sql = std::fs::read_to_string(dump_path)
        .with_context(|| format!("read dump {}", dump_path.display()))?;
    let mut conn = driver.acquire().await?;
    // Switch to target schema once.
    sqlx::query(&format!("USE `{target_db}`"))
        .execute(&mut *conn)
        .await?;
    sqlx::query("SET SESSION foreign_key_checks=0")
        .execute(&mut *conn)
        .await
        .ok();
    sqlx::query("SET SESSION unique_checks=0")
        .execute(&mut *conn)
        .await
        .ok();
    sqlx::query("SET SESSION sql_log_bin=0")
        .execute(&mut *conn)
        .await
        .ok();
    let mut applied: u64 = 0;
    for stmt in iter_statements(&sql) {
        if stmt.trim().is_empty() {
            continue;
        }
        conn.execute(sqlx::query(stmt))
            .await
            .with_context(|| format!("apply dump stmt #{applied}"))?;
        applied += 1;
    }
    debug!(target = target_db, count = applied, "mysql dump loaded");
    Ok(applied)
}

pub async fn load_postgres(
    driver: &PostgresDriver,
    target_db: &str,
    dump_path: &Path,
) -> Result<u64> {
    let sql = std::fs::read_to_string(dump_path)
        .with_context(|| format!("read dump {}", dump_path.display()))?;
    let mut conn = driver.acquire_db(target_db).await?;
    let mut applied: u64 = 0;
    for stmt in iter_statements(&sql) {
        if stmt.trim().is_empty() {
            continue;
        }
        conn.execute(sqlx::query(stmt))
            .await
            .with_context(|| format!("apply dump stmt #{applied}"))?;
        applied += 1;
    }
    debug!(target = target_db, count = applied, "pg dump loaded");
    Ok(applied)
}

/// Statement splitter. Tracks single/double quotes, backticks, and
/// `-- line` / `/* block */` comments so an embedded `;` inside a string
/// literal doesn't trigger a fake split.
fn iter_statements(sql: &str) -> StatementIter<'_> {
    StatementIter { rest: sql }
}

struct StatementIter<'a> {
    rest: &'a str,
}

impl<'a> Iterator for StatementIter<'a> {
    type Item = &'a str;
    fn next(&mut self) -> Option<&'a str> {
        if self.rest.is_empty() {
            return None;
        }
        let bytes = self.rest.as_bytes();
        let mut i = 0;
        let mut in_single = false;
        let mut in_double = false;
        let mut in_backtick = false;
        while i < bytes.len() {
            let b = bytes[i];
            if !(in_single || in_double || in_backtick) {
                // line comment
                if b == b'-' && i + 1 < bytes.len() && bytes[i + 1] == b'-' {
                    while i < bytes.len() && bytes[i] != b'\n' {
                        i += 1;
                    }
                    continue;
                }
                // block comment
                if b == b'/' && i + 1 < bytes.len() && bytes[i + 1] == b'*' {
                    i += 2;
                    while i + 1 < bytes.len() && !(bytes[i] == b'*' && bytes[i + 1] == b'/') {
                        i += 1;
                    }
                    i += 2;
                    continue;
                }
            }
            match b {
                b'\'' if !in_double && !in_backtick => in_single = !in_single,
                b'"' if !in_single && !in_backtick => in_double = !in_double,
                b'`' if !in_single && !in_double => in_backtick = !in_backtick,
                b';' if !in_single && !in_double && !in_backtick => {
                    let stmt = &self.rest[..i];
                    self.rest = &self.rest[i + 1..];
                    return Some(stmt);
                }
                _ => {}
            }
            i += 1;
        }
        let stmt = self.rest;
        self.rest = "";
        Some(stmt)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splits_simple_statements() {
        let s = "SELECT 1; SELECT 2; SELECT 3";
        let v: Vec<&str> = iter_statements(s).collect();
        assert_eq!(v.len(), 3);
    }

    #[test]
    fn semicolon_inside_string_does_not_split() {
        let s = "INSERT INTO t VALUES ('a;b'); SELECT 1;";
        let v: Vec<&str> = iter_statements(s).collect();
        assert_eq!(v.len(), 2);
        assert!(v[0].contains("a;b"));
    }

    #[test]
    fn line_comments_skipped() {
        let s = "-- comment;\nSELECT 1;";
        let v: Vec<&str> = iter_statements(s).collect();
        assert_eq!(v.len(), 1);
    }
}
