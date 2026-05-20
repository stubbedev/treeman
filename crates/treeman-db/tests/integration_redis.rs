//! Live Redis integration tests via testcontainers.

#![cfg(feature = "integration")]

use testcontainers_modules::redis::Redis;
use testcontainers_modules::testcontainers::runners::AsyncRunner;
use treeman_core::config::RedisConn;
use treeman_db::redis_driver::RedisDriver;
use treeman_db::{DbDriver, Namespace};

async fn start_redis() -> (
    testcontainers_modules::testcontainers::ContainerAsync<Redis>,
    RedisConn,
) {
    let container = Redis::default().start().await.expect("start redis");
    let port = container.get_host_port_ipv4(6379).await.expect("port");
    let cfg = RedisConn {
        url: format!("redis://127.0.0.1:{port}/"),
    };
    (container, cfg)
}

#[tokio::test]
#[ignore = "requires docker"]
async fn flush_indexed_db() {
    let (_c, cfg) = start_redis().await;
    let drv = RedisDriver::connect(&cfg).expect("connect");
    // Write into db 3 then flush it.
    let client =
        redis::Client::open(format!("{}3", cfg.url.trim_end_matches('/'))).expect("client");
    let mut conn = client
        .get_multiplexed_async_connection()
        .await
        .expect("multiplexed");
    use redis::AsyncCommands as _;
    let _: () = conn.set("k", "v").await.unwrap();
    drv.flush_namespace(&Namespace::RedisDb(3))
        .await
        .expect("flush");
    let v: Option<String> = conn.get("k").await.unwrap();
    assert!(v.is_none(), "key should be gone after FLUSHDB");
}
