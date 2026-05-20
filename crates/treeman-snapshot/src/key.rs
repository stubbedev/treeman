use std::collections::BTreeMap;
use std::path::Path;

use anyhow::Result;
use serde::{Deserialize, Serialize};

const FORMAT_VERSION: u32 = 1;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct SnapshotKey {
    pub format_version: u32,
    pub engine: String,
    pub engine_version: String,
    pub source_db_name: String,
    pub framework: String,
    pub hash_mode: String,
    pub migrations_hash_hex: String,
    pub dump_hash_hex: Option<String>,
    pub lockfile_hashes: BTreeMap<String, String>,
}

impl SnapshotKey {
    pub fn new(
        engine: &str,
        engine_version: &str,
        source_db: &str,
        framework: &str,
        hash_mode: &str,
        migrations_hash_hex: String,
        dump_hash_hex: Option<String>,
        lockfile_hashes: BTreeMap<String, String>,
    ) -> Self {
        Self {
            format_version: FORMAT_VERSION,
            engine: engine.into(),
            engine_version: engine_version.into(),
            source_db_name: source_db.into(),
            framework: framework.into(),
            hash_mode: hash_mode.into(),
            migrations_hash_hex,
            dump_hash_hex,
            lockfile_hashes,
        }
    }

    pub fn fingerprint(&self) -> String {
        let bytes = bincode::serde::encode_to_vec(self, bincode::config::standard())
            .expect("snapshot key serializable");
        blake3::hash(&bytes).to_hex().to_string()
    }

    pub fn template_name(&self) -> String {
        format!("_tm_tmpl_{}_{}", self.engine, &self.fingerprint()[..16])
    }
}

pub fn lockfile_hashes_for<P: AsRef<Path>>(paths: &[P]) -> Result<BTreeMap<String, String>> {
    let mut out = BTreeMap::new();
    for p in paths {
        let p = p.as_ref();
        if !p.is_file() { continue; }
        let bytes = std::fs::read(p)?;
        let hash = blake3::hash(&bytes).to_hex().to_string();
        let key = p.file_name().map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_else(|| p.display().to_string());
        out.insert(key, hash);
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fingerprint_changes_on_input_change() {
        let mut k = SnapshotKey::new(
            "mysql", "8.0.30", "kontainer_testing_kon_1234",
            "laravel", "filename",
            "abc123".into(), None, Default::default());
        let f1 = k.fingerprint();
        k.migrations_hash_hex = "def456".into();
        let f2 = k.fingerprint();
        assert_ne!(f1, f2);
    }

    #[test]
    fn template_name_includes_engine_and_prefix() {
        let k = SnapshotKey::new(
            "postgres", "16", "myapp_test",
            "rails", "filename",
            "h".into(), None, Default::default());
        let n = k.template_name();
        assert!(n.starts_with("_tm_tmpl_postgres_"));
        assert_eq!(n.len(), "_tm_tmpl_postgres_".len() + 16);
    }
}
