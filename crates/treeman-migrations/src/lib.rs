//! Migration-framework detection registry.
//!
//! 14 built-in detectors + YAML-declared custom frameworks (merged from
//! `Config::frameworks`). Detection runs at `repo.register`; the per-
//! framework hash mode + on-modify policy feed the watcher dispatch
//! logic in `treeman-watcher`.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use anyhow::Result;
use globset::GlobSet;
use serde::{Deserialize, Serialize};
use treeman_core::config::{CustomFramework, HashMode as CfgHashMode, OnModify as CfgOnModify};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HashMode { Filename, Checksum }

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OnModify { Rebuild, Delta }

impl From<CfgHashMode> for HashMode {
    fn from(c: CfgHashMode) -> Self {
        match c { CfgHashMode::Filename => Self::Filename, CfgHashMode::Checksum => Self::Checksum }
    }
}
impl From<CfgOnModify> for OnModify {
    fn from(c: CfgOnModify) -> Self {
        match c { CfgOnModify::Rebuild => Self::Rebuild, CfgOnModify::Delta => Self::Delta }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct FrameworkSpec {
    pub name: String,
    pub markers: Vec<String>,
    pub migration_dir_patterns: Vec<String>,
    pub file_globs: Vec<String>,
    pub lockfiles: Vec<String>,
    pub hash_mode: HashMode,
    pub on_modify: OnModify,
    pub engine_hint: Option<String>,
}

impl FrameworkSpec {
    pub fn detect(&self, repo_root: &Path) -> bool {
        if self.markers.is_empty() { return false; }
        self.markers.iter().all(|m| repo_root.join(m).exists())
    }

    /// Existing migration directories on disk that match
    /// `migration_dir_patterns`.
    pub fn migration_dirs(&self, repo_root: &Path) -> Vec<PathBuf> {
        let mut out = vec![];
        for p in &self.migration_dir_patterns {
            // Treat patterns without a `*` as literal paths for speed.
            if !p.contains('*') && !p.contains('?') {
                let dir = repo_root.join(p);
                if dir.is_dir() { out.push(dir); }
            } else {
                // Walk + glob.
                if let Ok(set) = build_globset(&[p.clone()]) {
                    for e in walkdir::WalkDir::new(repo_root).follow_links(false) {
                        let Ok(e) = e else { continue };
                        if !e.file_type().is_dir() { continue; }
                        let rel = e.path().strip_prefix(repo_root).unwrap_or(e.path());
                        if set.is_match(rel) { out.push(e.path().to_path_buf()); }
                    }
                }
            }
        }
        out
    }

    pub fn migration_files(&self, dir: &Path) -> Vec<PathBuf> {
        let Ok(set) = build_globset(&self.file_globs) else { return vec![]; };
        let mut out = vec![];
        for e in walkdir::WalkDir::new(dir).follow_links(false) {
            let Ok(e) = e else { continue };
            if !e.file_type().is_file() { continue; }
            let name = match e.file_name().to_str() { Some(n) => n, None => continue };
            if set.is_match(name) { out.push(e.path().to_path_buf()); }
        }
        out.sort();
        out
    }

    pub fn lockfile_paths(&self, repo_root: &Path) -> Vec<PathBuf> {
        self.lockfiles.iter()
            .map(|l| repo_root.join(l))
            .filter(|p| p.is_file())
            .collect()
    }

    pub fn hash_inputs(&self, files: &[PathBuf]) -> Result<MigrationHashSet> {
        let mut by_key = BTreeMap::new();
        for f in files {
            let sha = file_sha(f)?;
            match self.hash_mode {
                HashMode::Filename => {
                    let bn = f.file_name().map(|s| s.to_string_lossy().into_owned())
                        .unwrap_or_default();
                    by_key.insert(bn, sha);
                }
                HashMode::Checksum => {
                    by_key.insert(sha.clone(), sha);
                }
            }
        }
        Ok(MigrationHashSet { by_key, mode: self.hash_mode })
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MigrationHashSet {
    pub by_key: BTreeMap<String, String>,
    pub mode: HashMode,
}

fn build_globset(patterns: &[String]) -> Result<GlobSet> {
    let mut b = globset::GlobSetBuilder::new();
    for p in patterns {
        b.add(globset::Glob::new(p)?);
    }
    Ok(b.build()?)
}

fn file_sha(p: &Path) -> Result<String> {
    let bytes = std::fs::read(p)?;
    Ok(blake3::hash(&bytes).to_hex().to_string())
}

/// Registry holds every detector — built-ins followed by YAML-declared
/// custom frameworks. `detect_all` returns the specs whose markers exist
/// in the repo root; the watcher then iterates them in order.
pub struct Registry {
    pub specs: Vec<FrameworkSpec>,
}

impl Registry {
    pub fn with_builtins() -> Self {
        Self { specs: builtins() }
    }
    pub fn merge_yaml(mut self, yaml: &BTreeMap<String, CustomFramework>) -> Self {
        for (name, c) in yaml {
            let mut migration_dirs = c.migration_dirs.clone();
            // Inject globs containing `*` if the user used shell-glob syntax.
            for d in &mut migration_dirs { /* already strings */ let _ = d; }
            let spec = FrameworkSpec {
                name: name.clone(),
                markers: c.markers.clone(),
                migration_dir_patterns: migration_dirs,
                file_globs: vec![c.file_pattern.clone()],
                lockfiles: c.lockfiles.clone(),
                hash_mode: c.hash_mode.into(),
                on_modify: c.on_modify.into(),
                engine_hint: c.engine_hint.clone(),
            };
            // Same-name override removes the built-in.
            self.specs.retain(|s| s.name != *name);
            self.specs.push(spec);
        }
        self
    }
    pub fn detect_all(&self, repo_root: &Path) -> Vec<&FrameworkSpec> {
        self.specs.iter().filter(|s| s.detect(repo_root)).collect()
    }
}

fn builtins() -> Vec<FrameworkSpec> {
    use HashMode::*;
    use OnModify::*;
    fn fw(
        name: &str,
        markers: &[&str],
        dirs: &[&str],
        globs: &[&str],
        lockfiles: &[&str],
        hash_mode: HashMode,
        on_modify: OnModify,
        engine_hint: Option<&str>,
    ) -> FrameworkSpec {
        FrameworkSpec {
            name: name.into(),
            markers: markers.iter().map(|s| (*s).into()).collect(),
            migration_dir_patterns: dirs.iter().map(|s| (*s).into()).collect(),
            file_globs: globs.iter().map(|s| (*s).into()).collect(),
            lockfiles: lockfiles.iter().map(|s| (*s).into()).collect(),
            hash_mode, on_modify,
            engine_hint: engine_hint.map(|s| s.into()),
        }
    }
    vec![
        fw("laravel",
           &["artisan"], &["database/migrations"], &["*.php"],
           &["composer.lock"], Filename, Rebuild, Some("mysql")),
        fw("rails",
           &["bin/rails", "Gemfile", "config/database.yml"],
           &["db/migrate"], &["*.rb"], &["Gemfile.lock"], Filename, Rebuild, None),
        fw("django",
           &["manage.py"], &["**/migrations"], &["[0-9]*_*.py"],
           &["Pipfile.lock", "poetry.lock", "requirements.txt"],
           Filename, Rebuild, None),
        fw("golang-migrate",
           &["go.mod"], &["**/migrations"], &["*.up.sql"],
           &["go.sum"], Filename, Rebuild, None),
        fw("sqlx-cli",
           &["Cargo.toml", "migrations"], &["migrations"], &["*.sql"],
           &["Cargo.lock"], Checksum, Delta, None),
        fw("diesel",
           &["diesel.toml"], &["migrations"], &["up.sql"],
           &["Cargo.lock"], Filename, Rebuild, None),
        fw("prisma",
           &["prisma/schema.prisma"], &["prisma/migrations"], &["migration.sql"],
           &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
           Checksum, Delta, None),
        fw("knex",
           &["knexfile.js"], &["migrations"], &["*.js"],
           &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
           Filename, Rebuild, None),
        fw("alembic",
           &["alembic.ini"], &["**/versions"], &["*.py"],
           &["poetry.lock", "Pipfile.lock", "requirements.txt"],
           Filename, Rebuild, None),
        fw("flyway",
           &["flyway.conf"], &["**/db/migration"], &["[VRU]*.sql"],
           &[], Checksum, Rebuild, None),
        fw("typeorm",
           &["package.json"], &["src/migrations"], &["*.ts"],
           &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
           Filename, Rebuild, None),
        fw("drizzle",
           &["drizzle.config.ts"], &["drizzle"], &["*.sql"],
           &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
           Checksum, Delta, None),
        fw("sequelize",
           &[".sequelizerc"], &["migrations"], &["*.js"],
           &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
           Filename, Rebuild, None),
        fw("mikro-orm",
           &["mikro-orm.config.ts"], &["src/migrations"], &["Migration*.ts"],
           &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
           Filename, Rebuild, None),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn builtins_have_unique_names() {
        let r = Registry::with_builtins();
        let mut names: Vec<_> = r.specs.iter().map(|s| s.name.clone()).collect();
        names.sort();
        let len = names.len();
        names.dedup();
        assert_eq!(names.len(), len, "duplicate framework name");
    }

    #[test]
    fn detect_laravel_in_tempdir() {
        let dir = std::env::temp_dir().join(format!("treeman-laravel-{}",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("artisan"), "").unwrap();
        let r = Registry::with_builtins();
        let detected = r.detect_all(&dir);
        assert!(detected.iter().any(|s| s.name == "laravel"));
        std::fs::remove_dir_all(&dir).ok();
    }
}
