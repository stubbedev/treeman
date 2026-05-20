//! Snapshot/template engine. A fingerprint over (engine, engine_version,
//! framework hash_mode, migrations_hash, dump_hash, lockfile_hashes)
//! keys a per-source-db template that subsequent `prepare` invocations
//! restore in O(seconds).

pub mod key;
pub mod store;

pub use key::{SnapshotKey, lockfile_hashes_for};
pub use store::{SnapshotRow, gc_lru, list, mark_used, record_built};
