//! Snapshot/template engine. A fingerprint over (engine, engine_version,
//! framework hash_mode, migrations_hash, dump_hash, lockfile_hashes)
//! keys a per-source-db template that subsequent `prepare` invocations
//! restore in O(seconds).

pub mod gc;
pub mod key;
pub mod paratest;
pub mod store;

pub use gc::{GcReport, run_gc};
pub use key::{SnapshotKey, lockfile_hashes_for};
pub use paratest::{
    Engine as ParatestEngine, ParatestPlan, auto_clones, mysql_fanout, postgres_fanout,
};
pub use store::{SnapshotRow, gc_lru, list, mark_used, record_built};
