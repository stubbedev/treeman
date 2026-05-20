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

/// Drive prepare across all SQL databases configured for the repo.
pub async fn run(
    cfg: &Config,
    repo_root: &Path,
    slug: &Slug,
    sqlite: &SqlitePool,
    repo_id: i64,
    worktree_id: i64,
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
                run_migration(&m.framework, repo_root, source_db, MigrateMode::Up, &[]).await?;
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
                run_migration(&m.framework, repo_root, source_db, MigrateMode::Up, &[]).await?;
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
