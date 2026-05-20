//! Invoke a framework's migration command for a given target database.
//!
//! Frameworks read connection settings from their own config (Laravel
//! `.env`, Rails `config/database.yml`, etc.) so the runner exports
//! framework-appropriate env vars to override the target DB name.

use std::collections::BTreeMap;
use std::path::Path;
use std::process::Stdio;

use anyhow::{Context, Result, bail};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Command;

#[derive(Debug, Clone)]
pub struct RunOutcome {
    pub exit_code: i32,
    pub stdout_tail: String,
    pub stderr_tail: String,
}

#[derive(Debug, Clone, Copy)]
pub enum MigrateMode {
    /// Run all pending migrations from scratch.
    Up,
    /// Apply newly-added migrations only (delta).
    Pending,
}

/// Run the framework's migrate command against `target_db`. Connection
/// host/port/user/password come from `conn_env_overrides`.
///
/// `inherited_env` is the env map captured at CLI invocation. Hooks
/// and migrate subprocesses run with this map as their full env
/// (clearing the daemon's own env first) so a hook like
/// `php artisan migrate` finds `php` on the user's `$PATH` rather
/// than the minimal systemd-user env the daemon inherited.
pub async fn run(
    framework: &str,
    repo_root: &Path,
    target_db: &str,
    mode: MigrateMode,
    conn_env_overrides: &[(String, String)],
    inherited_env: &BTreeMap<String, String>,
) -> Result<RunOutcome> {
    let (cmd, args) = command_for(framework, mode)?;
    let mut c = Command::new(cmd);
    c.args(&args).current_dir(repo_root);

    // Replace the (potentially minimal) daemon env with the caller's.
    // Layer the framework knobs + `conn_env_overrides` on top so they
    // win over anything in `inherited_env`.
    c.env_clear();
    for (k, v) in inherited_env {
        c.env(k, v);
    }
    c.env("TREEMAN_TARGET_DB", target_db);

    // Framework-specific env override knobs. We avoid editing files —
    // every supported framework respects either DB_DATABASE / DATABASE_URL
    // / PGDATABASE / etc., so we point them at `target_db`.
    apply_framework_env(&mut c, framework, target_db);
    for (k, v) in conn_env_overrides {
        c.env(k, v);
    }

    let mut child = c
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .with_context(|| format!("spawn `{cmd} {}`", args.join(" ")))?;
    let so = tokio::spawn(capture_tail(child.stdout.take().expect("piped"), 16 * 1024));
    let se = tokio::spawn(capture_tail(child.stderr.take().expect("piped"), 16 * 1024));
    let status = child.wait().await?;
    Ok(RunOutcome {
        exit_code: status.code().unwrap_or(-1),
        stdout_tail: so.await.unwrap_or_default(),
        stderr_tail: se.await.unwrap_or_default(),
    })
}

fn command_for(framework: &str, mode: MigrateMode) -> Result<(&'static str, Vec<String>)> {
    let force = match mode {
        MigrateMode::Up => true,
        MigrateMode::Pending => true,
    };
    let v: (&'static str, Vec<String>) = match framework {
        "laravel" => ("php", vec_str(&["artisan", "migrate", "--force"])),
        "rails" => ("bin/rails", vec_str(&["db:migrate"])),
        "django" => ("python", vec_str(&["manage.py", "migrate", "--noinput"])),
        "golang-migrate" => ("migrate", vec_str(&["up"])),
        "sqlx-cli" => ("sqlx", vec_str(&["migrate", "run"])),
        "diesel" => ("diesel", vec_str(&["migration", "run"])),
        "prisma" => ("npx", vec_str(&["prisma", "migrate", "deploy"])),
        "knex" => ("npx", vec_str(&["knex", "migrate:latest"])),
        "alembic" => ("alembic", vec_str(&["upgrade", "head"])),
        "flyway" => ("flyway", vec_str(&["migrate"])),
        "typeorm" => ("npx", vec_str(&["typeorm", "migration:run"])),
        "drizzle" => ("npx", vec_str(&["drizzle-kit", "migrate"])),
        "sequelize" => ("npx", vec_str(&["sequelize", "db:migrate"])),
        "mikro-orm" => ("npx", vec_str(&["mikro-orm", "migration:up"])),
        other => bail!("unknown migration framework: {other}"),
    };
    let _ = force;
    Ok(v)
}

fn apply_framework_env(c: &mut Command, framework: &str, target_db: &str) {
    match framework {
        "laravel" => {
            // Laravel reads .env / .env.testing. Setting DB_DATABASE
            // overrides whichever connection the framework picks.
            c.env("DB_DATABASE", target_db);
            // For artisan test the `testing` connection often uses
            // DB_TEST_DATABASE; set both so the same call works in
            // dev + test contexts.
            c.env("DB_TEST_DATABASE", target_db);
        }
        "rails" => {
            // Rails 6+ honors DATABASE_URL fully, but ActiveRecord also
            // reads `RAILS_DATABASE` for the database name.
            c.env("DATABASE", target_db);
        }
        "django" => {
            c.env("DJANGO_DB_NAME", target_db);
        }
        "golang-migrate" => {
            // golang-migrate uses -database CLI flag; injecting via env
            // requires the caller to template DATABASE_URL.
            c.env("MIGRATE_DATABASE_NAME", target_db);
        }
        "sqlx-cli" => {
            c.env("DATABASE_URL_NAME", target_db);
        }
        "alembic" | "flyway" | "prisma" | "knex" | "typeorm" | "drizzle" | "sequelize"
        | "mikro-orm" | "diesel" => {
            // Generic env knob — frameworks that read DATABASE_URL must
            // be configured to interpolate ${TREEMAN_TARGET_DB}.
            c.env("DATABASE_NAME", target_db);
        }
        _ => {}
    }
}

async fn capture_tail<R: tokio::io::AsyncRead + Unpin + Send + 'static>(
    r: R,
    cap_bytes: usize,
) -> String {
    let mut reader = BufReader::new(r);
    let mut buf = Vec::with_capacity(cap_bytes.min(8 * 1024));
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
        buf.extend_from_slice(line.as_bytes());
        if buf.len() > cap_bytes {
            let drop_n = buf.len() - cap_bytes;
            buf.drain(0..drop_n);
        }
    }
    String::from_utf8_lossy(&buf).to_string()
}

fn vec_str(s: &[&str]) -> Vec<String> {
    s.iter().map(|x| (*x).to_string()).collect()
}
