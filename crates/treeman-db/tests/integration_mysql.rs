//! Live MySQL integration tests via testcontainers.
//!
//! `cargo test --features integration -p treeman-db --test integration_mysql -- --include-ignored`
//!
//! Each test is `#[ignore]` so the default `cargo test` invocation
//! doesn't pull container images on developer machines / on CI runners
//! that lack Docker.

#![cfg(feature = "integration")]

use testcontainers_modules::mysql::Mysql;
use testcontainers_modules::testcontainers::runners::AsyncRunner;
use treeman_core::config::MysqlConn;
use treeman_db::DbDriver;
use treeman_db::mysql::MysqlDriver;

async fn start_mysql() -> (
    testcontainers_modules::testcontainers::ContainerAsync<Mysql>,
    MysqlConn,
) {
    let container = Mysql::default().start().await.expect("start mysql");
    let port = container.get_host_port_ipv4(3306).await.expect("host port");
    let cfg = MysqlConn {
        host: "127.0.0.1".into(),
        port,
        user: "root".into(),
        password_env: None,
        pool_max: 4,
    };
    (container, cfg)
}

#[tokio::test]
#[ignore = "requires docker"]
async fn ensure_and_drop_db() {
    let (_c, cfg) = start_mysql().await;
    let drv = MysqlDriver::connect(&cfg).await.expect("connect");
    drv.ensure_db("tm_test_a").await.expect("ensure a");
    drv.ensure_db("tm_test_b").await.expect("ensure b");
    let matched = drv.list_matching("tm_test_").await.expect("list");
    assert!(matched.contains(&"tm_test_a".to_string()));
    assert!(matched.contains(&"tm_test_b".to_string()));
    let dropped = drv.drop_matching("tm_test_").await.expect("drop");
    assert_eq!(dropped.len(), 2);
    let after = drv.list_matching("tm_test_").await.expect("list");
    assert!(after.is_empty());
}

#[tokio::test]
#[ignore = "requires docker"]
async fn snapshot_roundtrip() {
    let (_c, cfg) = start_mysql().await;
    let drv = MysqlDriver::connect(&cfg).await.expect("connect");
    drv.ensure_db("tm_src").await.expect("ensure src");
    let pool = sqlx::MySqlPool::connect(&format!(
        "mysql://{}:@{}:{}/tm_src",
        cfg.user, cfg.host, cfg.port
    ))
    .await
    .expect("sqlx pool");
    sqlx::query("CREATE TABLE foo (id INT PRIMARY KEY, v TEXT)")
        .execute(&pool)
        .await
        .unwrap();
    sqlx::query("INSERT INTO foo VALUES (1, 'hello'), (2, 'world')")
        .execute(&pool)
        .await
        .unwrap();
    drv.snapshot_create("tm_src", "tm_tmpl")
        .await
        .expect("snapshot create");
    drv.snapshot_restore("tm_tmpl", "tm_dst")
        .await
        .expect("snapshot restore");
    let dst_pool = sqlx::MySqlPool::connect(&format!(
        "mysql://{}:@{}:{}/tm_dst",
        cfg.user, cfg.host, cfg.port
    ))
    .await
    .unwrap();
    let row: (i32, String) = sqlx::query_as("SELECT id, v FROM foo WHERE id = 1")
        .fetch_one(&dst_pool)
        .await
        .expect("row in cloned db");
    assert_eq!(row, (1, "hello".to_string()));
}
