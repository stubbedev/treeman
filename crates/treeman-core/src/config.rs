//! Layered config: global (`~/.config/treeman/config.yaml`) overlaid by
//! per-repo (`.treeman.yaml`) overlaid by per-repo-local (`.treeman.local.yaml`).
//!
//! The full schema (engines, snapshots, watchers, paratest) lands in later
//! milestones; M1 covers the slug + env-scoping subset needed by `treeman
//! slug` / `treeman config validate`.

use std::path::{Path, PathBuf};

use figment::Figment;
use figment::providers::{Format, Yaml};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Default, Serialize, Deserialize, JsonSchema)]
pub struct Config {
    #[serde(default)]
    pub daemon: DaemonConfig,
    #[serde(default)]
    pub connections: ConnectionsConfig,
    #[serde(default)]
    pub snapshots: SnapshotsConfig,
    #[serde(default)]
    pub repo: Option<RepoBlock>,
    #[serde(default)]
    pub slug: SlugRules,
    #[serde(default)]
    pub worktrees: WorktreesConfig,
    #[serde(default)]
    pub env_scoping: EnvScoping,
    #[serde(default)]
    pub databases: Vec<DatabaseConfig>,
    #[serde(default)]
    pub hooks: HooksConfig,
    #[serde(default)]
    pub watcher: WatcherConfig,
    #[serde(default)]
    pub frameworks: std::collections::BTreeMap<String, CustomFramework>,
}

/// Re-exports for the subset the daemon needs at boot.
pub type GlobalConfig = Config;
pub type RepoConfig = Config;

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct DaemonConfig {
    #[serde(default)]
    pub socket: Option<String>,
    #[serde(default = "default_log_level")]
    pub log_level: String,
    #[serde(default)]
    pub db_log_path: Option<PathBuf>,
}
impl Default for DaemonConfig {
    fn default() -> Self {
        Self { socket: None, log_level: default_log_level(), db_log_path: None }
    }
}
fn default_log_level() -> String { "info".into() }

#[derive(Debug, Clone, Default, Serialize, Deserialize, JsonSchema)]
pub struct ConnectionsConfig {
    #[serde(default)]
    pub mysql: Option<MysqlConn>,
    #[serde(default)]
    pub postgres: Option<PostgresConn>,
    #[serde(default)]
    pub mongodb: Option<MongoConn>,
    #[serde(default)]
    pub redis: Option<RedisConn>,
    #[serde(default)]
    pub elasticsearch: Option<EsConn>,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct MysqlConn {
    pub host: String,
    pub port: u16,
    pub user: String,
    #[serde(default)]
    pub password_env: Option<String>,
    #[serde(default = "default_pool")]
    pub pool_max: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct PostgresConn {
    pub host: String,
    pub port: u16,
    pub user: String,
    #[serde(default)]
    pub password_env: Option<String>,
    #[serde(default = "default_pool")]
    pub pool_max: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct MongoConn {
    pub uri: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct RedisConn {
    pub url: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct EsConn {
    pub url: String,
}

fn default_pool() -> u32 { 8 }

#[derive(Debug, Clone, Default, Serialize, Deserialize, JsonSchema)]
pub struct SnapshotsConfig {
    #[serde(default)]
    pub cache_dir: Option<PathBuf>,
    #[serde(default)]
    pub retention: RetentionConfig,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct RetentionConfig {
    #[serde(default = "default_keep_per_source")]
    pub keep_per_source: u32,
    #[serde(default = "default_max_age_days")]
    pub max_age_days: u32,
    #[serde(default = "default_max_total_gb")]
    pub max_total_gb: u32,
}
impl Default for RetentionConfig {
    fn default() -> Self {
        Self {
            keep_per_source: default_keep_per_source(),
            max_age_days: default_max_age_days(),
            max_total_gb: default_max_total_gb(),
        }
    }
}
fn default_keep_per_source() -> u32 { 5 }
fn default_max_age_days() -> u32 { 30 }
fn default_max_total_gb() -> u32 { 50 }

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct RepoBlock {
    pub name: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct SlugRules {
    #[serde(default = "default_ticket_regex")]
    pub ticket_regex: String,
    #[serde(default = "default_fallback")]
    pub fallback: String,
}
impl Default for SlugRules {
    fn default() -> Self {
        Self {
            ticket_regex: default_ticket_regex(),
            fallback: default_fallback(),
        }
    }
}
fn default_ticket_regex() -> String { r"^([A-Z]+)-(\d+)".into() }
fn default_fallback() -> String { "wt_{shorthash8}".into() }

#[derive(Debug, Clone, Default, Serialize, Deserialize, JsonSchema)]
pub struct WorktreesConfig {
    #[serde(default = "default_worktrees_root")]
    pub root: String,
    #[serde(default)]
    pub links: Vec<String>,
}
fn default_worktrees_root() -> String { "../worktrees".into() }

#[derive(Debug, Clone, Default, Serialize, Deserialize, JsonSchema)]
pub struct EnvScoping {
    #[serde(default)]
    pub files: Vec<String>,
    #[serde(default = "default_true")]
    pub skip_worktree: bool,
    #[serde(default)]
    pub patches: Vec<EnvPatch>,
}
fn default_true() -> bool { true }

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct EnvPatch {
    pub key: String,
    pub template: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
#[serde(tag = "engine", rename_all = "lowercase")]
pub enum DatabaseConfig {
    Mysql {
        name_template: String,
        #[serde(default)]
        dump: Option<DumpSpec>,
        #[serde(default)]
        migrations: Option<MigrationSpec>,
        #[serde(default)]
        paratest: Option<ParatestSpec>,
    },
    Postgres {
        name_template: String,
        #[serde(default)]
        dump: Option<DumpSpec>,
        #[serde(default)]
        migrations: Option<MigrationSpec>,
        #[serde(default)]
        paratest: Option<ParatestSpec>,
    },
    Mongodb {
        name_template: String,
    },
    Redis {
        namespaces: RedisNamespaces,
    },
    Elasticsearch {
        namespaces: EsNamespaces,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct DumpSpec {
    pub path: PathBuf,
    #[serde(default)]
    pub optional: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct MigrationSpec {
    pub framework: String,
    #[serde(default)]
    pub dir: Option<PathBuf>,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct ParatestSpec {
    #[serde(default = "default_clones")]
    pub clones: ClonesSetting,
    pub name_template: String,
}
fn default_clones() -> ClonesSetting { ClonesSetting::Auto }

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
#[serde(untagged)]
pub enum ClonesSetting {
    #[serde(rename = "auto")]
    Auto,
    Fixed(u32),
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct RedisNamespaces {
    pub db_index_template: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct EsNamespaces {
    pub index_prefix_template: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, JsonSchema)]
pub struct HooksConfig {
    #[serde(default)]
    pub precreate: Vec<HookStep>,
    #[serde(default)]
    pub postcreate: Vec<HookStep>,
    #[serde(default)]
    pub predelete: Vec<HookStep>,
    #[serde(default)]
    pub postdelete: Vec<HookStep>,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct HookStep {
    pub run: String,
    #[serde(default)]
    pub background: bool,
    #[serde(default)]
    pub cwd: Option<PathBuf>,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct WatcherConfig {
    #[serde(default)]
    pub paths: Vec<WatcherPath>,
    #[serde(default = "default_debounce_ms")]
    pub debounce_ms: u64,
}
impl Default for WatcherConfig {
    fn default() -> Self {
        Self { paths: vec![], debounce_ms: default_debounce_ms() }
    }
}
fn default_debounce_ms() -> u64 { 500 }

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct WatcherPath {
    pub glob: String,
    #[serde(default)]
    pub on: WatcherAction,
}

#[derive(Debug, Clone, Copy, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum WatcherAction {
    #[default]
    Auto,
    Delta,
    Rebuild,
}

#[derive(Debug, Clone, Serialize, Deserialize, JsonSchema)]
pub struct CustomFramework {
    pub markers: Vec<String>,
    pub migration_dirs: Vec<String>,
    pub file_pattern: String,
    #[serde(default)]
    pub hash_mode: HashMode,
    #[serde(default)]
    pub on_modify: OnModify,
    #[serde(default)]
    pub lockfiles: Vec<String>,
    #[serde(default)]
    pub engine_hint: Option<String>,
}

#[derive(Debug, Clone, Copy, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum HashMode {
    #[default]
    Filename,
    Checksum,
}

#[derive(Debug, Clone, Copy, Default, Serialize, Deserialize, JsonSchema)]
#[serde(rename_all = "snake_case")]
pub enum OnModify {
    #[default]
    Rebuild,
    Delta,
}

/// Layered load:
///   1. global  = $XDG_CONFIG_HOME/treeman/config.yaml (or ~/.config/...)
///   2. repo    = <repo_root>/.treeman.yaml
///   3. local   = <repo_root>/.treeman.local.yaml
/// Later layers override earlier. Missing files are ignored.
pub fn load_layered(repo_root: Option<&Path>) -> anyhow::Result<Config> {
    let mut fig = Figment::new();
    if let Some(global) = global_path() {
        if global.exists() {
            fig = fig.merge(Yaml::file(&global));
        }
    }
    if let Some(repo) = repo_root {
        let r = repo.join(".treeman.yaml");
        if r.exists() {
            fig = fig.merge(Yaml::file(&r));
        }
        let l = repo.join(".treeman.local.yaml");
        if l.exists() {
            fig = fig.merge(Yaml::file(&l));
        }
    }
    Ok(fig.extract::<Config>()?)
}

pub fn global_path() -> Option<PathBuf> {
    let dirs = directories::ProjectDirs::from("", "", "treeman")?;
    Some(dirs.config_local_dir().join("config.yaml"))
}

/// Generate the JSON Schema for the full config (for editor LSP / `treeman
/// schema dump`).
pub fn json_schema() -> serde_json::Value {
    let schema = schemars::schema_for!(Config);
    serde_json::to_value(schema).expect("schema serializable")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_yaml_yields_default_config() {
        let tmp = tempfile_helper("");
        let cfg = Figment::new()
            .merge(Yaml::file(&tmp))
            .extract::<Config>()
            .unwrap();
        assert_eq!(cfg.daemon.log_level, "info");
    }

    #[test]
    fn schema_emits_non_empty() {
        let v = json_schema();
        let s = serde_json::to_string(&v).unwrap();
        assert!(s.contains("\"Config\""));
    }

    fn tempfile_helper(content: &str) -> PathBuf {
        use std::io::Write;
        let mut p = std::env::temp_dir();
        p.push(format!("treeman-test-{}.yaml", uuid_like()));
        let mut f = std::fs::File::create(&p).unwrap();
        f.write_all(content.as_bytes()).unwrap();
        p
    }

    fn uuid_like() -> String {
        blake3::hash(format!("{:?}", std::time::Instant::now()).as_bytes())
            .to_hex()
            .to_string()
    }
}
