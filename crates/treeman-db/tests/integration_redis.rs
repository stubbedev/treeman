//! Live Redis integration tests. Uses `TREEMAN_TEST_REDIS_URL` when
//! set, otherwise spins up `testcontainers` Redis. Test scopes to a
//! deterministically-chosen DB index (1..15).

#![cfg(feature = "integration")]

use testcontainers_modules::redis::Redis;
use testcontainers_modules::testcontainers::ContainerAsync;
use testcontainers_modules::testcontainers::runners::AsyncRunner;
use treeman_core::config::RedisConn;
use treeman_db::redis_driver::RedisDriver;
use treeman_db::{DbDriver, Namespace};

#[allow(dead_code, clippy::large_enum_variant)]
enum Backing {
    Env,
    Container(ContainerAsync<Redis>),
}

async fn backing() -> (Backing, RedisConn, u8) {
    if let Ok(url) = std::env::var("TREEMAN_TEST_REDIS_URL") {
        let idx = (blake3::hash(url.as_bytes()).as_bytes()[0] % 15) + 1;
        return (Backing::Env, RedisConn { url }, idx);
    }
    let container = Redis::default().start().await.expect("start redis");
    let port = container.get_host_port_ipv4(6379).await.expect("port");
    let cfg = RedisConn {
        url: format!("redis://127.0.0.1:{port}/"),
    };
    (Backing::Container(container), cfg, 1)
}

#[tokio::test]
#[ignore = "live Redis — env or docker"]
async fn flush_indexed_db() {
    let (_b, cfg, idx) = backing().await;
    let drv = RedisDriver::connect(&cfg).expect("connect");
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
