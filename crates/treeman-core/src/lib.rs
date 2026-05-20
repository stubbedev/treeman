//! Treeman core. Pure logic — no tokio, no sqlite, no network.

pub mod config;
pub mod patcher;
pub mod slug;
pub mod template;

pub use config::{Config, GlobalConfig, RepoConfig, load_layered};
pub use patcher::{PatchOutcome, patch_env_file, patch_phpunit_file};
pub use slug::{Slug, slug_for};
