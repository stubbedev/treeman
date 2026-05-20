//! Migration-framework detection registry.
//!
//! 14 built-in detectors + YAML-declared custom frameworks (merged from
//! `Config::frameworks`). Detection runs at `repo.register`; the per-
//! framework hash mode + on-modify policy feed the watcher dispatch
//! logic in `treeman-watcher`.

pub mod runner;
pub mod testfw;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use anyhow::Result;
use globset::GlobSet;
use serde::{Deserialize, Serialize};
use treeman_core::config::{CustomFramework, HashMode as CfgHashMode, OnModify as CfgOnModify};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HashMode {
    Filename,
    Checksum,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OnModify {
    Rebuild,
    Delta,
}

impl From<CfgHashMode> for HashMode {
    fn from(c: CfgHashMode) -> Self {
        match c {
            CfgHashMode::Filename => Self::Filename,
            CfgHashMode::Checksum => Self::Checksum,
        }
    }
}
impl From<CfgOnModify> for OnModify {
    fn from(c: CfgOnModify) -> Self {
        match c {
            CfgOnModify::Rebuild => Self::Rebuild,
            CfgOnModify::Delta => Self::Delta,
        }
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
    /// All marker groups must match. A marker group is a single string;
    /// a `|`-delimited string means "any of these files satisfies the
    /// group" (used to match config files that come in `.ts`, `.js`,
    /// `.cjs`, `.mjs` variants without inflating the AND-list).
    ///
    /// Examples:
    ///   "artisan"                                  — file must exist
    ///   "knexfile.js|knexfile.ts|knexfile.cjs"     — any of the three
    pub fn detect(&self, repo_root: &Path) -> bool {
        if self.markers.is_empty() {
            return false;
        }
        self.markers.iter().all(|m| {
            m.split('|')
                .any(|alt| !alt.is_empty() && repo_root.join(alt.trim()).exists())
        })
    }

    /// Existing migration directories on disk that match
    /// `migration_dir_patterns`. Walks the repo once and matches each
    /// glob against every directory entry, pruning heavy subtrees
    /// (.git / node_modules / vendor / target / __pycache__ / etc.).
    /// Literal (glob-free) patterns short-circuit without walking.
    pub fn migration_dirs(&self, repo_root: &Path) -> Vec<PathBuf> {
        let mut out: Vec<PathBuf> = vec![];

        // Literal patterns first — cheap, no walk.
        let mut glob_patterns: Vec<String> = vec![];
        for p in &self.migration_dir_patterns {
            if p.contains('*') || p.contains('?') {
                glob_patterns.push(p.clone());
            } else {
                let dir = repo_root.join(p);
                if dir.is_dir() {
                    out.push(dir);
                }
            }
        }
        if glob_patterns.is_empty() {
            dedup_sorted(&mut out);
            return out;
        }
        let Ok(set) = build_globset(&glob_patterns) else {
            dedup_sorted(&mut out);
            return out;
        };

        // Single walk for all glob patterns together. Prune heavy
        // dirs via filter_entry so a vendor/ or node_modules/ doesn't
        // turn detection into a multi-GB stat-fest.
        let iter = walkdir::WalkDir::new(repo_root)
            .follow_links(false)
            .into_iter()
            .filter_entry(|e| {
                if e.depth() == 0 {
                    return true;
                }
                let name = e.file_name().to_string_lossy();
                !is_skip_dir(name.as_ref())
            });
        for e in iter {
            let Ok(e) = e else { continue };
            if !e.file_type().is_dir() {
                continue;
            }
            let rel = e.path().strip_prefix(repo_root).unwrap_or(e.path());
            if set.is_match(rel) {
                out.push(e.path().to_path_buf());
            }
        }
        dedup_sorted(&mut out);
        out
    }

    pub fn migration_files(&self, dir: &Path) -> Vec<PathBuf> {
        let Ok(set) = build_globset(&self.file_globs) else {
            return vec![];
        };
        let mut out = vec![];
        let iter = walkdir::WalkDir::new(dir)
            .follow_links(false)
            .into_iter()
            .filter_entry(|e| {
                if e.depth() == 0 {
                    return true;
                }
                let name = e.file_name().to_string_lossy();
                !is_skip_dir(name.as_ref())
            });
        for e in iter {
            let Ok(e) = e else { continue };
            if !e.file_type().is_file() {
                continue;
            }
            let name = match e.file_name().to_str() {
                Some(n) => n,
                None => continue,
            };
            if set.is_match(name) {
                out.push(e.path().to_path_buf());
            }
        }
        out.sort();
        out
    }

    /// Directories the file-watcher should `notify::watch` recursively
    /// so that *new* module/monorepo migration dirs created mid-watch
    /// also fire events. For a glob pattern like
    /// `app/Modules/*/Database/Migrations`, this returns the deepest
    /// existing ancestor of `app/Modules` so a freshly-created
    /// `app/Modules/NewMod/Database/Migrations/2024_01_foo.php` triggers
    /// the recursive watcher.
    ///
    /// Returned set is deduped and ancestor-merged: if `app/Modules` is
    /// in the set, `app/Modules/Foo/Database/Migrations` is dropped.
    pub fn watch_roots(&self, repo_root: &Path) -> Vec<PathBuf> {
        let mut roots: Vec<PathBuf> = vec![];
        for pat in &self.migration_dir_patterns {
            if pat.contains('*') || pat.contains('?') {
                // Prefix before the first wildcard.
                let head = pat.split(['*', '?']).next().unwrap_or("");
                let head = head.trim_end_matches('/');
                // `is_broad` = pattern starts with the wildcard itself
                // (`**/migrations`, `*foo`). These are intentionally
                // broad — fall back to repo_root if no existing
                // ancestor is found, so Django/Alembic/Flyway projects
                // with no apps yet still get events when `manage.py
                // startapp foo` runs.
                let is_broad = head.is_empty();
                let mut candidate = repo_root.join(head);
                loop {
                    // Check repo_root BEFORE is_dir — repo_root is always
                    // a dir but we only want to use it as a watch root
                    // for broad ** patterns. For finite globs whose
                    // ancestor doesn't exist (e.g. Rails `engines/*/...`
                    // with no engines/ dir) we drop the entry silently.
                    if candidate == repo_root {
                        if is_broad {
                            roots.push(candidate);
                        }
                        break;
                    }
                    if candidate.is_dir() {
                        roots.push(candidate);
                        break;
                    }
                    if !candidate.starts_with(repo_root) {
                        break;
                    }
                    match candidate.parent() {
                        Some(p) => candidate = p.to_path_buf(),
                        None => break,
                    }
                }
            } else {
                let dir = repo_root.join(pat);
                if dir.is_dir() {
                    roots.push(dir);
                }
            }
        }
        roots.sort();
        roots.dedup();
        let mut merged: Vec<PathBuf> = vec![];
        for r in roots {
            if merged.iter().any(|m| r.starts_with(m)) {
                continue;
            }
            merged.retain(|m| !m.starts_with(&r) || m == &r);
            merged.push(r);
        }
        merged
    }

    pub fn lockfile_paths(&self, repo_root: &Path) -> Vec<PathBuf> {
        self.lockfiles
            .iter()
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
                    let bn = f
                        .file_name()
                        .map(|s| s.to_string_lossy().into_owned())
                        .unwrap_or_default();
                    by_key.insert(bn, sha);
                }
                HashMode::Checksum => {
                    by_key.insert(sha.clone(), sha);
                }
            }
        }
        Ok(MigrationHashSet {
            by_key,
            mode: self.hash_mode,
        })
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

/// Directory names pruned from migration_dirs / migration_files walks.
/// Anything generated, vendored, or known not to host hand-written
/// migrations. Keeps detection O(repo source size), not O(repo+deps).
fn is_skip_dir(name: &str) -> bool {
    matches!(
        name,
        ".git"
            | ".hg"
            | ".svn"
            | "node_modules"
            | ".pnpm-store"
            | ".yarn"
            | "vendor"
            | "target"
            | "build"
            | "dist"
            | "out"
            | ".venv"
            | "venv"
            | "__pycache__"
            | ".tox"
            | ".idea"
            | ".vscode"
            | ".next"
            | ".nuxt"
            | ".cache"
            | ".gradle"
            | ".m2"
            | "tmp"
    )
}

fn dedup_sorted(v: &mut Vec<PathBuf>) {
    v.sort();
    v.dedup();
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
            for d in &mut migration_dirs {
                /* already strings */
                let _ = d;
            }
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
    #[allow(clippy::too_many_arguments)]
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
            hash_mode,
            on_modify,
            engine_hint: engine_hint.map(|s| s.into()),
        }
    }
    vec![
        // Laravel — canonical + nwidart/laravel-modules (app/Modules and
        // legacy top-level Modules/) + lowercase variants.
        fw(
            "laravel",
            &["artisan"],
            &[
                "database/migrations",
                "app/Modules/*/Database/Migrations",
                "app/Modules/*/Database/migrations",
                "Modules/*/Database/Migrations",
                "Modules/*/Database/migrations",
            ],
            &["*.php"],
            &["composer.lock"],
            Filename,
            Rebuild,
            Some("mysql"),
        ),
        // Rails — canonical + engines (mountable engines hold their own db/migrate).
        fw(
            "rails",
            &["bin/rails", "Gemfile", "config/database.yml"],
            &["db/migrate", "engines/*/db/migrate"],
            &["*.rb"],
            &["Gemfile.lock"],
            Filename,
            Rebuild,
            None,
        ),
        // Django — every app has its own */migrations folder.
        fw(
            "django",
            &["manage.py"],
            &["**/migrations"],
            &["[0-9]*_*.py"],
            &["Pipfile.lock", "poetry.lock", "requirements.txt"],
            Filename,
            Rebuild,
            None,
        ),
        // golang-migrate — single + Go monorepo (services / cmd).
        fw(
            "golang-migrate",
            &["go.mod"],
            &["**/migrations", "services/*/migrations", "cmd/*/migrations"],
            &["*.up.sql"],
            &["go.sum"],
            Filename,
            Rebuild,
            None,
        ),
        // sqlx-cli — single + cargo workspace crates / services.
        fw(
            "sqlx-cli",
            &["Cargo.toml", "migrations"],
            &["migrations", "crates/*/migrations", "services/*/migrations"],
            &["*.sql"],
            &["Cargo.lock"],
            Checksum,
            Delta,
            None,
        ),
        // Diesel — single + cargo workspace.
        fw(
            "diesel",
            &["diesel.toml"],
            &["migrations", "crates/*/migrations"],
            &["up.sql"],
            &["Cargo.lock"],
            Filename,
            Rebuild,
            None,
        ),
        // Prisma — single + NX / yarn monorepo (apps + packages).
        fw(
            "prisma",
            &["prisma/schema.prisma"],
            &[
                "prisma/migrations",
                "apps/*/prisma/migrations",
                "packages/*/prisma/migrations",
            ],
            &["migration.sql"],
            &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
            Checksum,
            Delta,
            None,
        ),
        // Knex — single + monorepo. knexfile lives at the repo root
        // with .js / .ts / .cjs / .mjs variants depending on the
        // project's module system (CommonJS vs ESM vs TS).
        fw(
            "knex",
            &["knexfile.js|knexfile.ts|knexfile.cjs|knexfile.mjs"],
            &["migrations", "apps/*/migrations", "packages/*/migrations"],
            &["*.js", "*.ts"],
            &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
            Filename,
            Rebuild,
            None,
        ),
        // Alembic — every package can have versions/.
        fw(
            "alembic",
            &["alembic.ini"],
            &["**/versions"],
            &["*.py"],
            &["poetry.lock", "Pipfile.lock", "requirements.txt"],
            Filename,
            Rebuild,
            None,
        ),
        // Flyway — already a glob; covers Spring Boot multi-module too.
        fw(
            "flyway",
            &["flyway.conf"],
            &["**/db/migration"],
            &["[VRU]*.sql"],
            &[],
            Checksum,
            Rebuild,
            None,
        ),
        // TypeORM — single + NX/yarn monorepo. Detect by the TypeORM
        // DataSource config file, not just package.json (which would
        // match every JS project). The canonical name shifted over
        // versions: `ormconfig.{json,js,ts,yaml}` for typeorm 0.2,
        // `data-source.ts` for 0.3+ (CLI default), plus user-named
        // `typeorm.config.ts`.
        fw(
            "typeorm",
            &[
                "ormconfig.json|ormconfig.js|ormconfig.ts|ormconfig.yaml|ormconfig.yml|data-source.ts|data-source.js|typeorm.config.ts|typeorm.config.js",
            ],
            &[
                "src/migrations",
                "src/migration",
                "migrations",
                "apps/*/src/migrations",
                "packages/*/src/migrations",
            ],
            &["*.ts", "*.js"],
            &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
            Filename,
            Rebuild,
            None,
        ),
        // Drizzle — single + monorepo. drizzle.config can be any of
        // .ts/.js/.mjs/.cjs/.mts; older `drizzle.config.json` exists
        // for purely JSON-driven setups.
        fw(
            "drizzle",
            &[
                "drizzle.config.ts|drizzle.config.js|drizzle.config.mjs|drizzle.config.cjs|drizzle.config.mts|drizzle.config.json",
            ],
            &["drizzle", "apps/*/drizzle", "packages/*/drizzle"],
            &["*.sql"],
            &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
            Checksum,
            Delta,
            None,
        ),
        // Sequelize — single + monorepo. .sequelizerc is conventional;
        // can also be a `.js`/`.cjs` factory file.
        fw(
            "sequelize",
            &[".sequelizerc|.sequelizerc.js|.sequelizerc.cjs"],
            &["migrations", "apps/*/migrations", "packages/*/migrations"],
            &["*.js", "*.ts"],
            &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
            Filename,
            Rebuild,
            None,
        ),
        // MikroORM — single + monorepo. config file may be .ts/.js/
        // .cjs depending on module system.
        fw(
            "mikro-orm",
            &["mikro-orm.config.ts|mikro-orm.config.js|mikro-orm.config.cjs"],
            &[
                "src/migrations",
                "apps/*/src/migrations",
                "packages/*/src/migrations",
            ],
            &["Migration*.ts", "Migration*.js"],
            &["package-lock.json", "pnpm-lock.yaml", "yarn.lock"],
            Filename,
            Rebuild,
            None,
        ),
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
    fn watch_roots_picks_existing_ancestor_for_glob() {
        let dir = std::env::temp_dir().join(format!(
            "treeman-watch-roots-{}",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(dir.join("app/Modules")).unwrap();
        std::fs::write(dir.join("artisan"), "").unwrap();
        std::fs::create_dir_all(dir.join("database/migrations")).unwrap();
        // No modules yet — but app/Modules exists. Watcher should still
        // watch app/Modules so the first `module:make` triggers events.

        let r = Registry::with_builtins();
        let spec = r.specs.iter().find(|s| s.name == "laravel").unwrap();
        let roots: Vec<String> = spec
            .watch_roots(&dir)
            .iter()
            .map(|p| p.strip_prefix(&dir).unwrap().display().to_string())
            .collect();
        assert!(
            roots.contains(&"database/migrations".to_string()),
            "expected canonical dir watched, got {roots:?}"
        );
        assert!(
            roots.contains(&"app/Modules".to_string()),
            "expected app/Modules watched (empty modules dir), got {roots:?}"
        );
        // Modules/ does not exist — should NOT appear (we walk up; parent
        // would be repo_root, but that's only used as a last-resort).
        assert!(
            !roots.iter().any(|r| r.is_empty()),
            "should not fall all the way back to repo_root, got {roots:?}"
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn watch_roots_broad_pattern_falls_back_to_repo_root() {
        // Django-style: `**/migrations` with no apps yet.
        let dir = std::env::temp_dir().join(format!(
            "treeman-broad-{}",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("manage.py"), "").unwrap();
        let r = Registry::with_builtins();
        let spec = r.specs.iter().find(|s| s.name == "django").unwrap();
        let roots = spec.watch_roots(&dir);
        let canonical = std::fs::canonicalize(&dir).unwrap();
        assert!(
            roots.iter().any(|r| r == &dir || r == &canonical),
            "broad ** pattern with no app dirs should fall back to repo_root, got {roots:?}"
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn watch_roots_finite_glob_drops_when_ancestor_missing() {
        // Rails-style: `engines/*/db/migrate` with no `engines/` dir.
        // Should NOT fall back to repo_root (would be too broad).
        let dir = std::env::temp_dir().join(format!(
            "treeman-finite-{}",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(dir.join("db/migrate")).unwrap();
        std::fs::create_dir_all(dir.join("bin")).unwrap();
        std::fs::create_dir_all(dir.join("config")).unwrap();
        std::fs::write(dir.join("bin/rails"), "").unwrap();
        std::fs::write(dir.join("Gemfile"), "").unwrap();
        std::fs::write(dir.join("config/database.yml"), "").unwrap();
        let r = Registry::with_builtins();
        let spec = r.specs.iter().find(|s| s.name == "rails").unwrap();
        let roots = spec.watch_roots(&dir);
        let canonical = std::fs::canonicalize(&dir).unwrap();
        // Should contain db/migrate but NOT repo_root itself.
        assert!(
            !roots.iter().any(|r| r == &dir || r == &canonical),
            "finite glob with missing ancestor must not fall back to repo_root, got {roots:?}"
        );
        assert!(
            roots.iter().any(|r| r.ends_with("db/migrate")),
            "expected db/migrate watched, got {roots:?}"
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn watch_roots_dedupes_nested_ancestors() {
        let dir = std::env::temp_dir().join(format!(
            "treeman-ancestor-merge-{}",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(dir.join("app/Modules/Foo/Database/Migrations")).unwrap();
        std::fs::write(dir.join("artisan"), "").unwrap();
        let r = Registry::with_builtins();
        let spec = r.specs.iter().find(|s| s.name == "laravel").unwrap();
        let roots = spec.watch_roots(&dir);
        // Both app/Modules (from the glob root) and
        // app/Modules/Foo/Database/Migrations (from migration_dirs)
        // exist, but watch_roots should NOT include the deeper one once
        // the ancestor is present.
        let strs: Vec<String> = roots
            .iter()
            .map(|p| p.strip_prefix(&dir).unwrap().display().to_string())
            .collect();
        let nested = strs.iter().any(|s| s.starts_with("app/Modules/Foo"));
        assert!(!nested, "ancestor merge failed: {strs:?}");
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn laravel_finds_module_migration_dirs() {
        let dir = std::env::temp_dir().join(format!(
            "treeman-laravel-mod-{}",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("artisan"), "").unwrap();
        std::fs::create_dir_all(dir.join("database/migrations")).unwrap();
        std::fs::create_dir_all(dir.join("app/Modules/Foo/Database/Migrations")).unwrap();
        std::fs::create_dir_all(dir.join("app/Modules/Bar/Database/migrations")).unwrap();
        std::fs::create_dir_all(dir.join("Modules/Legacy/Database/Migrations")).unwrap();
        // Heavy dirs should be pruned, not matched.
        std::fs::create_dir_all(dir.join("vendor/somepkg/Database/Migrations")).unwrap();
        std::fs::create_dir_all(dir.join("node_modules/foo/database/migrations")).unwrap();

        let r = Registry::with_builtins();
        let spec = r.specs.iter().find(|s| s.name == "laravel").unwrap();
        let mut dirs: Vec<String> = spec
            .migration_dirs(&dir)
            .iter()
            .map(|p| p.strip_prefix(&dir).unwrap().display().to_string())
            .collect();
        dirs.sort();
        assert!(dirs.contains(&"database/migrations".to_string()));
        assert!(dirs.contains(&"app/Modules/Foo/Database/Migrations".to_string()));
        assert!(dirs.contains(&"app/Modules/Bar/Database/migrations".to_string()));
        assert!(dirs.contains(&"Modules/Legacy/Database/Migrations".to_string()));
        // Pruned heavy dirs:
        assert!(!dirs.iter().any(|d| d.starts_with("vendor")));
        assert!(!dirs.iter().any(|d| d.starts_with("node_modules")));
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn detect_laravel_in_tempdir() {
        let dir = std::env::temp_dir().join(format!(
            "treeman-laravel-{}",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("artisan"), "").unwrap();
        let r = Registry::with_builtins();
        let detected = r.detect_all(&dir);
        assert!(detected.iter().any(|s| s.name == "laravel"));
        std::fs::remove_dir_all(&dir).ok();
    }

    fn fresh_tempdir(label: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!(
            "treeman-{label}-{}",
            blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// Plain JS project (just `package.json`) must NOT match
    /// TypeORM / Drizzle / Sequelize / MikroORM / Knex — those were
    /// previously triggered by the loose `package.json`-only marker.
    #[test]
    fn plain_js_project_does_not_match_orms() {
        let dir = fresh_tempdir("plain-js");
        std::fs::write(dir.join("package.json"), r#"{"name":"plain"}"#).unwrap();
        std::fs::write(dir.join("yarn.lock"), "").unwrap();
        let r = Registry::with_builtins();
        let names: Vec<&str> = r.detect_all(&dir).iter().map(|s| s.name.as_str()).collect();
        for orm in [
            "typeorm",
            "drizzle",
            "sequelize",
            "mikro-orm",
            "knex",
            "prisma",
        ] {
            assert!(
                !names.contains(&orm),
                "{orm} matched a plain JS project — detector too loose. matched: {names:?}"
            );
        }
        std::fs::remove_dir_all(&dir).ok();
    }

    /// TypeORM project (data-source.ts) matches; plain package.json
    /// alongside doesn't make any other ORM match.
    #[test]
    fn typeorm_matches_only_on_datasource_config() {
        let dir = fresh_tempdir("typeorm");
        std::fs::write(dir.join("package.json"), r#"{"name":"app"}"#).unwrap();
        std::fs::write(dir.join("data-source.ts"), "export default ...").unwrap();
        let r = Registry::with_builtins();
        let names: Vec<&str> = r.detect_all(&dir).iter().map(|s| s.name.as_str()).collect();
        assert!(names.contains(&"typeorm"), "got {names:?}");
        assert!(!names.contains(&"drizzle"), "got {names:?}");
        assert!(!names.contains(&"sequelize"), "got {names:?}");
        std::fs::remove_dir_all(&dir).ok();
    }

    /// Knex with a TS-style knexfile.
    #[test]
    fn knex_matches_typescript_config() {
        let dir = fresh_tempdir("knex-ts");
        std::fs::write(dir.join("knexfile.ts"), "export default {}").unwrap();
        let r = Registry::with_builtins();
        let names: Vec<&str> = r.detect_all(&dir).iter().map(|s| s.name.as_str()).collect();
        assert!(names.contains(&"knex"), "got {names:?}");
        std::fs::remove_dir_all(&dir).ok();
    }

    /// Drizzle with an mjs config.
    #[test]
    fn drizzle_matches_mjs_config() {
        let dir = fresh_tempdir("drizzle-mjs");
        std::fs::write(dir.join("drizzle.config.mjs"), "export default {}").unwrap();
        let r = Registry::with_builtins();
        let names: Vec<&str> = r.detect_all(&dir).iter().map(|s| s.name.as_str()).collect();
        assert!(names.contains(&"drizzle"), "got {names:?}");
        std::fs::remove_dir_all(&dir).ok();
    }
}
