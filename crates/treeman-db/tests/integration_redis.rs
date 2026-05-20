//! Live Redis integration tests against an existing instance.
//! Set `TREEMAN_TEST_REDIS_URL` (e.g. `redis://127.0.0.1:6379/`).
//! Each run scopes itself to a deterministically-chosen DB index
//! (computed from the URL) so concurrent runs against the same
//! instance don't trample each other.

#![cfg(feature = "integration")]

use treeman_core::config::RedisConn;
use treeman_db::redis_driver::RedisDriver;
use treeman_db::{DbDriver, Namespace};

fn env_cfg() -> Option<(RedisConn, u8)> {
    let url = std::env::var("TREEMAN_TEST_REDIS_URL").ok()?;
    let bytes = blake3::hash(url.as_bytes());
    let idx = (bytes.as_bytes()[0] % 15) + 1; // 1..=15, leave 0 alone
    Some((RedisConn { url }, idx))
}

#[tokio::test]
#[ignore = "set TREEMAN_TEST_REDIS_URL"]
async fn flush_indexed_db() {
    let Some((cfg, idx)) = env_cfg() else {
        eprintln!("TREEMAN_TEST_REDIS_URL not set — skipping");
        return;
    };
    let drv = RedisDriver::connect(&cfg).expect("connect");
    // Write into the scoped index, then flush it.
    let trimmed = cfg.url.trim_end_matches('/').to_string();
    let client = redis::Client::open(format!("{trimmed}/{idx}")).expect("client");
    let mut conn = client
        .get_multiplexed_async_connection()
        .await
        .expect("multiplexed");
    use redis::AsyncCommands as _;
    let _: () = conn.set("k", "v").await.unwrap();
    drv.flush_namespace(&Namespace::RedisDb(idx))
        .await
        .expect("flush");
    let v: Option<String> = conn.get("k").await.unwrap();
    assert!(v.is_none(), "key should be gone after FLUSHDB");
}
