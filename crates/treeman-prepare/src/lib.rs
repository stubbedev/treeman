//! `treeman prepare` orchestrator. Mirrors build_helper's `prepare`:
//!
//!   1. Render the scoped source DB name from the worktree slug.
//!   2. ensure_db(source).
//!   3. Compute the SnapshotKey (engine, framework, migrations_hash,
//!      dump_hash, lockfile_hashes).
//!   4. If a snapshot for fingerprint exists → restore template → source.
//!   5. Else: drop+recreate source, load dump (if configured), run
//!      migrations, snapshot_create(source, template), record_built.
//!   6. paratest fan-out into N clones from the template.

pub mod teardown;
pub use teardown::teardown_databases;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use sqlx::SqlitePool;
use tracing::info;

use treeman_core::config::{
    ClonesSetting, Config, DatabaseConfig, DumpSpec, MigrationSpec, ParatestSpec,
};
use treeman_core::slug::Slug;
use treeman_core::template::{TemplateContext, render};
use treeman_db::{DbDriver, mysql::MysqlDriver, postgres::PostgresDriver};
use treeman_migrations::{
    HashMode as MigHashMode, Registry,
    runner::{MigrateMode, run as run_migration},
};
use treeman_snapshot::{
    ParatestEngine, ParatestPlan, SnapshotKey, lockfile_hashes_for, mysql_fanout, postgres_fanout,
    record_built,
};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct PrepareOutcome {
    pub engine: String,
    pub source_db: String,
    pub template_name: String,
    pub fingerprint: String,
    pub cache_hit: bool,
    pub clones: Vec<String>,
}

/// Incremental "delta" path. Triggered by the watcher when
/// `Dispatch::Delta` fires (newly-added migration files only).
///
/// For mysql/mariadb/tidb: migrate the SOURCE DB once, then replay the
/// resulting binlog row events onto every paratest clone. ~N× faster
/// than re-running migrations on each clone for large `clones` counts.
/// Falls back to per-clone framework-migrate if the binlog stream
/// isn't usable (binlog off, source unreachable from where the daemon
/// lives, replication user lacks grants, etc.).
///
/// For postgres/cockroach: there is no equivalent of a wire-level
/// logical-replication stream that's universally enabled, so this
/// path always runs the framework's pending-only migrate on the
/// source AND each clone. Frameworks track applied migrations, so a
/// pending-only invocation is idempotent and still faster than a
/// full rebuild.
pub async fn delta_run(
    cfg: &Config,
    repo_root: &Path,
    slug: &Slug,
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
    inherited_env: &BTreeMap<String, String>,
) -> Result<Vec<DeltaOutcome>> {
    let ctx = TemplateContext::from_slug(slug);
    let mut outcomes = Vec::new();
    for d in &cfg.databases {
        let outcome = match d {
            DatabaseConfig::Mysql {
                name_template,
                migrations,
                paratest,
                ..
            }
            | DatabaseConfig::Mariadb {
                name_template,
                migrations,
                paratest,
                ..
            }
            | DatabaseConfig::Tidb {
                name_template,
                migrations,
                paratest,
                ..
            } => {
                let engine = engine_name_for(d);
                let Some(mspec) = migrations.as_ref() else {
                    info!(engine, "no migrations: skipping delta");
                    continue;
                };
                let source_db = render(name_template, &ctx)?;
                let clones = resolve_clone_names(paratest.as_ref(), slug, repo_root)?;
                run_delta_mysql(
                    cfg,
                    engine,
                    &source_db,
                    &clones,
                    mspec,
                    repo_root,
                    sqlite,
                    repo_id,
                    worktree_id,
                    inherited_env,
                )
                .await?
            }
            DatabaseConfig::Postgres {
                name_template,
                migrations,
                paratest,
                ..
            }
            | DatabaseConfig::Cockroach {
                name_template,
                migrations,
                paratest,
                ..
            } => {
                let engine = engine_name_for(d);
                let Some(mspec) = migrations.as_ref() else {
                    info!(engine, "no migrations: skipping delta");
                    continue;
                };
                let source_db = render(name_template, &ctx)?;
                let clones = resolve_clone_names(paratest.as_ref(), slug, repo_root)?;
                run_delta_remigrate(
                    engine,
                    &source_db,
                    &clones,
                    mspec,
                    repo_root,
                    sqlite,
                    repo_id,
                    worktree_id,
                    inherited_env,
                )
                .await?
            }
            _ => continue,
        };
        outcomes.push(outcome);
    }
    Ok(outcomes)
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeltaOutcome {
    pub engine: String,
    pub source_db: String,
    pub clones: Vec<String>,
    pub migrate_exit_codes: Vec<i32>,
}

fn engine_name_for(d: &DatabaseConfig) -> &'static str {
    match d {
        DatabaseConfig::Mysql { .. } => "mysql",
        DatabaseConfig::Mariadb { .. } => "mariadb",
        DatabaseConfig::Tidb { .. } => "tidb",
        DatabaseConfig::Postgres { .. } => "postgres",
        DatabaseConfig::Cockroach { .. } => "cockroach",
        _ => "other",
    }
}

fn resolve_clone_names(
    paratest: Option<&ParatestSpec>,
    slug: &Slug,
    repo_root: &Path,
) -> Result<Vec<String>> {
    let Some(p) = paratest else { return Ok(vec![]) };
    let n = match p.clones {
        ClonesSetting::Fixed(v) => v,
        ClonesSetting::Auto => treeman_migrations::testfw::detected_clone_count(repo_root)
            .unwrap_or_else(treeman_snapshot::auto_clones),
    };
    if n == 0 {
        return Ok(vec![]);
    }
    let base_ctx = TemplateContext::from_slug(slug);
    let mut out = Vec::with_capacity(n as usize);
    for i in 1..=n {
        let ctx = base_ctx.clone().with_n(i);
        out.push(render(&p.name_template, &ctx)?);
    }
    Ok(out)
}

/// Per-clone framework-migrate fallback. Idempotent thanks to the
/// migration framework's bookkeeping table.
#[allow(clippy::too_many_arguments)]
async fn run_delta_remigrate(
    engine: &'static str,
    source_db: &str,
    clones: &[String],
    mspec: &MigrationSpec,
    repo_root: &Path,
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
    inherited_env: &BTreeMap<String, String>,
) -> Result<DeltaOutcome> {
    let mut codes = Vec::new();
    // Source first — frameworks track state, so subsequent clone runs
    // pick up the same pending set.
    let out = run_migration(
        &mspec.framework,
        repo_root,
        source_db,
        MigrateMode::Pending,
        &[],
        inherited_env,
    )
    .await?;
    codes.push(out.exit_code);
    if out.exit_code != 0 {
        emit(
            sqlite,
            "error",
            "delta_migrate",
            engine,
            source_db,
            repo_id,
            worktree_id,
            &out.stderr_tail,
        )
        .await;
        anyhow::bail!(
            "delta migrate failed on source {source_db}: exit {} — {}",
            out.exit_code,
            out.stderr_tail
        );
    }

    for clone in clones {
        let out = run_migration(
            &mspec.framework,
            repo_root,
            clone,
            MigrateMode::Pending,
            &[],
            inherited_env,
        )
        .await?;
        codes.push(out.exit_code);
        if out.exit_code != 0 {
            emit(
                sqlite,
                "error",
                "delta_migrate",
                engine,
                clone,
                repo_id,
                worktree_id,
                &out.stderr_tail,
            )
            .await;
            anyhow::bail!(
                "delta migrate failed on clone {clone}: exit {} — {}",
                out.exit_code,
                out.stderr_tail
            );
        }
    }
    emit(
        sqlite,
        "info",
        "delta_done",
        engine,
        source_db,
        repo_id,
        worktree_id,
        &format!("clones={}; mode=remigrate", clones.len()),
    )
    .await;
    Ok(DeltaOutcome {
        engine: engine.into(),
        source_db: source_db.into(),
        clones: clones.to_vec(),
        migrate_exit_codes: codes,
    })
}

/// Mysql-family delta. Capture binlog position → run framework
/// migrate on source → replay binlog row events against every clone.
/// Any binlog-stream failure (engine unreachable, log_bin off,
/// replication grants missing, source DML not visible in binlog)
/// degrades cleanly to [`run_delta_remigrate`] so the watcher is
/// never blocked.
#[allow(clippy::too_many_arguments)]
async fn run_delta_mysql(
    cfg: &Config,
    engine: &'static str,
    source_db: &str,
    clones: &[String],
    mspec: &MigrationSpec,
    repo_root: &Path,
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
    inherited_env: &BTreeMap<String, String>,
) -> Result<DeltaOutcome> {
    use treeman_db::binlog::{BinlogError, BinlogReplicator};

    // No clones to feed → cheap path. Still migrate source so the
    // template/source DB stays current.
    if clones.is_empty() {
        return run_delta_remigrate(
            engine,
            source_db,
            clones,
            mspec,
            repo_root,
            sqlite,
            repo_id,
            worktree_id,
            inherited_env,
        )
        .await;
    }

    let Some(mc) = cfg.connections.mysql.clone() else {
        // No mysql connection resolved — can't open the binlog stream.
        // Fall back so the user still gets clones up-to-date.
        return run_delta_remigrate(
            engine,
            source_db,
            clones,
            mspec,
            repo_root,
            sqlite,
            repo_id,
            worktree_id,
            inherited_env,
        )
        .await;
    };

    let replicator = match BinlogReplicator::connect(&mc).await {
        Ok(r) => r,
        Err(e) => {
            emit(
                sqlite,
                "warn",
                "binlog_unavailable",
                engine,
                source_db,
                repo_id,
                worktree_id,
                &format!("falling back to per-clone migrate: {e}"),
            )
            .await;
            return run_delta_remigrate(
                engine,
                source_db,
                clones,
                mspec,
                repo_root,
                sqlite,
                repo_id,
                worktree_id,
                inherited_env,
            )
            .await;
        }
    };

    // Capture the pre-migration master position. If binlog is off the
    // call returns BinlogDisabled; fall back.
    let from = match replicator.current_position().await {
        Ok(p) => p,
        Err(e) => {
            emit(
                sqlite,
                "warn",
                "binlog_unavailable",
                engine,
                source_db,
                repo_id,
                worktree_id,
                &format!("falling back to per-clone migrate: {e}"),
            )
            .await;
            let _ = replicator.close().await;
            return run_delta_remigrate(
                engine,
                source_db,
                clones,
                mspec,
                repo_root,
                sqlite,
                repo_id,
                worktree_id,
                inherited_env,
            )
            .await;
        }
    };

    // Run pending migrations on the source only. Frameworks update
    // their bookkeeping rows + DDL/DML, all of which lands in the
    // binlog between `from` and the new master position.
    let mut codes = Vec::new();
    let out = run_migration(
        &mspec.framework,
        repo_root,
        source_db,
        MigrateMode::Pending,
        &[],
        inherited_env,
    )
    .await?;
    codes.push(out.exit_code);
    if out.exit_code != 0 {
        emit(
            sqlite,
            "error",
            "delta_migrate",
            engine,
            source_db,
            repo_id,
            worktree_id,
            &out.stderr_tail,
        )
        .await;
        let _ = replicator.close().await;
        anyhow::bail!(
            "delta migrate failed on source {source_db}: exit {} — {}",
            out.exit_code,
            out.stderr_tail
        );
    }

    // Replay BOTH DDL Query events and DML Row events between
    // `from` and the new master position. The replayer fans the
    // captured stream onto each clone, so we do NOT need to invoke
    // the framework's migrate command per-clone afterwards — the
    // schema delta, any seed inserts, and the framework's
    // bookkeeping table row are all carried by the stream.
    match replicator.replay_range(&from, source_db, clones).await {
        Ok(summary) => {
            emit(
                sqlite,
                "info",
                "binlog_replay",
                engine,
                source_db,
                repo_id,
                worktree_id,
                &format!(
                    "events_seen={} ddl_events_applied={} row_events_applied={} \
                     rows_applied={} clones={}",
                    summary.events_seen,
                    summary.ddl_events_applied,
                    summary.row_events_applied,
                    summary.rows_applied,
                    clones.len()
                ),
            )
            .await;
            let _ = replicator.close().await;
            emit(
                sqlite,
                "info",
                "delta_done",
                engine,
                source_db,
                repo_id,
                worktree_id,
                &format!("clones={}; mode=binlog", clones.len()),
            )
            .await;
            Ok(DeltaOutcome {
                engine: engine.into(),
                source_db: source_db.into(),
                clones: clones.to_vec(),
                migrate_exit_codes: codes,
            })
        }
        Err(BinlogError::Unreachable { .. } | BinlogError::BinlogDisabled) => {
            // Source state already advanced. Fall through to
            // per-clone framework migrate to bring clones up to date.
            emit(
                sqlite,
                "warn",
                "binlog_unavailable_midway",
                engine,
                source_db,
                repo_id,
                worktree_id,
                "falling back to per-clone migrate after source-migrate",
            )
            .await;
            let _ = replicator.close().await;
            run_remaining_clones(
                engine,
                source_db,
                clones,
                mspec,
                repo_root,
                sqlite,
                repo_id,
                worktree_id,
                codes,
                inherited_env,
            )
            .await
        }
        Err(e) => {
            let _ = replicator.close().await;
            anyhow::bail!("binlog replay failed for {source_db}: {e}");
        }
    }
}

/// Tail of `run_delta_mysql` reused when the binlog replay aborts
/// midway. We've already migrated the source; finish the clones via
/// framework migrate.
#[allow(clippy::too_many_arguments)]
async fn run_remaining_clones(
    engine: &'static str,
    source_db: &str,
    clones: &[String],
    mspec: &MigrationSpec,
    repo_root: &Path,
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
    mut codes: Vec<i32>,
    inherited_env: &BTreeMap<String, String>,
) -> Result<DeltaOutcome> {
    for clone in clones {
        let out = run_migration(
            &mspec.framework,
            repo_root,
            clone,
            MigrateMode::Pending,
            &[],
            inherited_env,
        )
        .await?;
        codes.push(out.exit_code);
        if out.exit_code != 0 {
            emit(
                sqlite,
                "error",
                "delta_migrate",
                engine,
                clone,
                repo_id,
                worktree_id,
                &out.stderr_tail,
            )
            .await;
            anyhow::bail!(
                "delta migrate failed on clone {clone}: exit {} — {}",
                out.exit_code,
                out.stderr_tail
            );
        }
    }
    emit(
        sqlite,
        "info",
        "delta_done",
        engine,
        source_db,
        repo_id,
        worktree_id,
        &format!("clones={}; mode=binlog+remigrate-fallback", clones.len()),
    )
    .await;
    Ok(DeltaOutcome {
        engine: engine.into(),
        source_db: source_db.into(),
        clones: clones.to_vec(),
        migrate_exit_codes: codes,
    })
}

#[allow(clippy::too_many_arguments)]
async fn emit(
    sqlite: &SqlitePool,
    level: &str,
    event_type: &str,
    engine: &str,
    target: &str,
    repo_id: i64,
    worktree_id: i64,
    message: &str,
) {
    let payload = serde_json::json!({ "engine": engine, "target": target }).to_string();
    let _ = treeman_store::write_event(
        sqlite,
        level,
        event_type,
        Some(message),
        Some(repo_id),
        Some(worktree_id),
        None,
        None,
        &payload,
    )
    .await;
}

/// Drive prepare across all SQL databases configured for the repo.
///
/// `inherited_env` is the env captured at CLI invocation and threaded
/// through the daemon RPC. Hook + framework-migrate subprocesses run
/// with this as their full env so `php` / `composer` / `yarn` /
/// `php artisan migrate` find the tools the user has on PATH, not
/// the daemon's minimal systemd-user env.
pub async fn run(
    cfg: &Config,
    repo_root: &Path,
    slug: &Slug,
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
    inherited_env: &BTreeMap<String, String>,
) -> Result<Vec<PrepareOutcome>> {
    let ctx = TemplateContext::from_slug(slug);
    let registry = Registry::with_builtins().merge_yaml(&cfg.frameworks);
    let mut outcomes = Vec::new();
    for d in &cfg.databases {
        match d {
            DatabaseConfig::Mysql {
                name_template,
                dump,
                migrations,
                paratest,
            } => {
                let source_db = render(name_template, &ctx)?;
                let mc = cfg
                    .connections
                    .mysql
                    .clone()
                    .context("connections.mysql not configured")?;
                let driver = Arc::new(MysqlDriver::connect(&mc).await?);
                outcomes.push(
                    prepare_mysql(
                        Arc::clone(&driver),
                        &source_db,
                        dump.as_ref(),
                        migrations.as_ref(),
                        paratest.as_ref(),
                        &registry,
                        repo_root,
                        sqlite,
                        repo_id,
                        worktree_id,
                        slug,
                        inherited_env,
                    )
                    .await?,
                );
            }
            DatabaseConfig::Postgres {
                name_template,
                dump,
                migrations,
                paratest,
            } => {
                let source_db = render(name_template, &ctx)?;
                let pc = cfg
                    .connections
                    .postgres
                    .clone()
                    .context("connections.postgres not configured")?;
                let driver = Arc::new(PostgresDriver::connect(&pc).await?);
                outcomes.push(
                    prepare_postgres(
                        Arc::clone(&driver),
                        &source_db,
                        dump.as_ref(),
                        migrations.as_ref(),
                        paratest.as_ref(),
                        &registry,
                        repo_root,
                        sqlite,
                        repo_id,
                        worktree_id,
                        slug,
                        inherited_env,
                    )
                    .await?,
                );
            }
            _ => continue,
        }
    }
    Ok(outcomes)
}

#[allow(clippy::too_many_arguments)]
async fn prepare_mysql(
    driver: Arc<MysqlDriver>,
    source_db: &str,
    dump: Option<&DumpSpec>,
    migration_spec: Option<&MigrationSpec>,
    paratest_spec: Option<&ParatestSpec>,
    registry: &Registry,
    repo_root: &Path,
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
    slug: &Slug,
    inherited_env: &BTreeMap<String, String>,
) -> Result<PrepareOutcome> {
    let key = build_key(
        "mysql",
        driver.as_ref(),
        source_db,
        dump,
        migration_spec,
        registry,
        repo_root,
    )
    .await?;
    let fingerprint = key.fingerprint();
    let template_name = key.template_name();
    let cache_hit = check_cache(sqlite, &fingerprint).await?;

    driver.ensure_db(source_db).await?;
    if cache_hit {
        info!(engine = "mysql", source_db, template = %template_name, "cache hit");
        driver.snapshot_restore(&template_name, source_db).await?;
        treeman_snapshot::mark_used(sqlite, &fingerprint).await?;
    } else {
        info!(engine = "mysql", source_db, "cold prepare");
        // Drop + recreate source.
        let _ = driver.drop_matching(source_db).await;
        driver.ensure_db(source_db).await?;
        if let Some(d) = dump {
            let dp = repo_root.join(&d.path);
            if dp.is_file() {
                treeman_db::dumpload::load_mysql(&driver, source_db, &dp)
                    .await
                    .with_context(|| format!("load dump {}", dp.display()))?;
            } else if !d.optional {
                anyhow::bail!("dump not found: {}", dp.display());
            }
        }
        if let Some(m) = migration_spec {
            let out =
                run_migration(&m.framework, repo_root, source_db, MigrateMode::Up, &[], inherited_env).await?;
            if out.exit_code != 0 {
                anyhow::bail!(
                    "migrate failed (exit {}): {}",
                    out.exit_code,
                    out.stderr_tail
                );
            }
        }
        driver.snapshot_create(source_db, &template_name).await?;
        record_built(sqlite, &key, &template_name, None).await?;
        emit_snapshot_built(
            sqlite,
            repo_id,
            worktree_id,
            "mysql",
            &fingerprint,
            &template_name,
        )
        .await;
    }

    let clones = if let Some(p) = paratest_spec {
        do_fanout_mysql(driver, &template_name, p, slug, repo_root).await?
    } else {
        vec![]
    };

    Ok(PrepareOutcome {
        engine: "mysql".into(),
        source_db: source_db.into(),
        template_name,
        fingerprint,
        cache_hit,
        clones,
    })
}

#[allow(clippy::too_many_arguments)]
async fn prepare_postgres(
    driver: Arc<PostgresDriver>,
    source_db: &str,
    dump: Option<&DumpSpec>,
    migration_spec: Option<&MigrationSpec>,
    paratest_spec: Option<&ParatestSpec>,
    registry: &Registry,
    repo_root: &Path,
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
    slug: &Slug,
    inherited_env: &BTreeMap<String, String>,
) -> Result<PrepareOutcome> {
    let key = build_key(
        "postgres",
        driver.as_ref(),
        source_db,
        dump,
        migration_spec,
        registry,
        repo_root,
    )
    .await?;
    let fingerprint = key.fingerprint();
    let template_name = key.template_name();
    let cache_hit = check_cache(sqlite, &fingerprint).await?;

    driver.ensure_db(source_db).await?;
    if cache_hit {
        info!(engine = "postgres", source_db, template = %template_name, "cache hit");
        driver.snapshot_restore(&template_name, source_db).await?;
        treeman_snapshot::mark_used(sqlite, &fingerprint).await?;
    } else {
        info!(engine = "postgres", source_db, "cold prepare");
        let _ = driver.drop_matching(source_db).await;
        driver.ensure_db(source_db).await?;
        if let Some(d) = dump {
            let dp = repo_root.join(&d.path);
            if dp.is_file() {
                treeman_db::dumpload::load_postgres(&driver, source_db, &dp)
                    .await
                    .with_context(|| format!("load dump {}", dp.display()))?;
            } else if !d.optional {
                anyhow::bail!("dump not found: {}", dp.display());
            }
        }
        if let Some(m) = migration_spec {
            let out =
                run_migration(&m.framework, repo_root, source_db, MigrateMode::Up, &[], inherited_env).await?;
            if out.exit_code != 0 {
                anyhow::bail!(
                    "migrate failed (exit {}): {}",
                    out.exit_code,
                    out.stderr_tail
                );
            }
        }
        driver.snapshot_create(source_db, &template_name).await?;
        record_built(sqlite, &key, &template_name, None).await?;
        emit_snapshot_built(
            sqlite,
            repo_id,
            worktree_id,
            "postgres",
            &fingerprint,
            &template_name,
        )
        .await;
    }

    let clones = if let Some(p) = paratest_spec {
        do_fanout_pg(driver, &template_name, p, slug, repo_root).await?
    } else {
        vec![]
    };

    Ok(PrepareOutcome {
        engine: "postgres".into(),
        source_db: source_db.into(),
        template_name,
        fingerprint,
        cache_hit,
        clones,
    })
}

async fn build_key<D: DbDriver + ?Sized>(
    engine: &str,
    driver: &D,
    source_db: &str,
    dump: Option<&DumpSpec>,
    migration_spec: Option<&MigrationSpec>,
    registry: &Registry,
    repo_root: &Path,
) -> Result<SnapshotKey> {
    let framework = migration_spec
        .map(|m| m.framework.as_str())
        .unwrap_or("none");
    let spec = registry.specs.iter().find(|s| s.name == framework);
    let engine_version = driver.engine_version().await?;
    let migration_files: Vec<PathBuf> = if let Some(s) = spec {
        let mut all = vec![];
        for d in s.migration_dirs(repo_root) {
            all.extend(s.migration_files(&d));
        }
        all.sort();
        all
    } else {
        vec![]
    };
    let migrations_hash_hex = blake3_hash_of_paths(&migration_files)?;
    let dump_hash_hex = match dump.map(|d| repo_root.join(&d.path)) {
        Some(p) if p.is_file() => Some(blake3_hash_of_paths(&[p])?),
        _ => None,
    };
    let lockfile_paths: Vec<PathBuf> = spec
        .map(|s| s.lockfile_paths(repo_root))
        .unwrap_or_default();
    let lockfile_hashes = lockfile_hashes_for(&lockfile_paths)?;
    let hash_mode_str = spec
        .map(|s| match s.hash_mode {
            MigHashMode::Filename => "filename",
            MigHashMode::Checksum => "checksum",
        })
        .unwrap_or("filename");

    Ok(SnapshotKey::new(
        engine,
        &engine_version,
        source_db,
        framework,
        hash_mode_str,
        migrations_hash_hex,
        dump_hash_hex,
        lockfile_hashes,
    ))
}

async fn check_cache(sqlite: &SqlitePool, fingerprint: &str) -> Result<bool> {
    let n: Option<i64> = sqlx::query_scalar("SELECT 1 FROM snapshots WHERE fingerprint = ?")
        .bind(fingerprint)
        .fetch_optional(sqlite)
        .await?;
    Ok(n.is_some())
}

async fn emit_snapshot_built(
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
    engine: &str,
    fingerprint: &str,
    template_name: &str,
) {
    let payload = serde_json::json!({
        "engine": engine, "fingerprint": fingerprint, "template": template_name
    })
    .to_string();
    let _ = treeman_store::write_event(
        sqlite,
        "info",
        "snapshot_built",
        Some(&format!("{engine}: built {template_name}")),
        Some(repo_id),
        Some(worktree_id),
        None,
        None,
        &payload,
    )
    .await;
}

async fn do_fanout_mysql(
    driver: Arc<MysqlDriver>,
    template_name: &str,
    p: &ParatestSpec,
    slug: &Slug,
    repo_root: &Path,
) -> Result<Vec<String>> {
    let n = resolve_clone_count(p, repo_root);
    if n == 0 {
        return Ok(vec![]);
    }
    let plan = ParatestPlan {
        engine: ParatestEngine::Mysql,
        source_db: template_name.into(),
        clones: n,
        name_template: p.name_template.clone(),
    };
    mysql_fanout(driver, plan, &slug.value).await
}

async fn do_fanout_pg(
    driver: Arc<PostgresDriver>,
    template_name: &str,
    p: &ParatestSpec,
    slug: &Slug,
    repo_root: &Path,
) -> Result<Vec<String>> {
    let n = resolve_clone_count(p, repo_root);
    if n == 0 {
        return Ok(vec![]);
    }
    let plan = ParatestPlan {
        engine: ParatestEngine::Postgres,
        source_db: template_name.into(),
        clones: n,
        name_template: p.name_template.clone(),
    };
    postgres_fanout(driver, plan, &slug.value).await
}

/// Resolve N from config + auto-detected test framework.
///
/// `ClonesSetting::Fixed(n)` always wins. `Auto` consults
/// `treeman_migrations::testfw::detected_clone_count`; if no test
/// framework is detected, falls back to num_cpus (preserves prior
/// behavior).
fn resolve_clone_count(p: &ParatestSpec, repo_root: &Path) -> u32 {
    match p.clones {
        ClonesSetting::Fixed(v) => v,
        ClonesSetting::Auto => treeman_migrations::testfw::detected_clone_count(repo_root)
            .unwrap_or_else(treeman_snapshot::auto_clones),
    }
}

fn blake3_hash_of_paths(paths: &[PathBuf]) -> Result<String> {
    let mut hasher = blake3::Hasher::new();
    for p in paths {
        if p.is_file() {
            let bytes = std::fs::read(p)?;
            hasher.update(p.to_string_lossy().as_bytes());
            hasher.update(b"\0");
            hasher.update(&bytes);
            hasher.update(b"\n");
        }
    }
    Ok(hasher.finalize().to_hex().to_string())
}
