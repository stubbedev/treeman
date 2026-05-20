//! Live PostgreSQL integration tests via testcontainers.

#![cfg(feature = "integration")]

use testcontainers_modules::postgres::Postgres;
use testcontainers_modules::testcontainers::runners::AsyncRunner;
use treeman_core::config::PostgresConn;
use treeman_db::DbDriver;
use treeman_db::postgres::PostgresDriver;

async fn start_postgres() -> (
    testcontainers_modules::testcontainers::ContainerAsync<Postgres>,
    PostgresConn,
) {
    let container = Postgres::default().start().await.expect("start pg");
    let port = container.get_host_port_ipv4(5432).await.expect("port");
    let cfg = PostgresConn {
        host: "127.0.0.1".into(),
        port,
        user: "postgres".into(),
        password_env: None,
        pool_max: 4,
    };
    (container, cfg)
}

#[tokio::test]
#[ignore = "requires docker"]
async fn ensure_and_drop_db() {
    let (_c, cfg) = start_postgres().await;
    let drv = PostgresDriver::connect(&cfg).await.expect("connect");
    drv.ensure_db("tm_test_a").await.expect("ensure a");
    drv.ensure_db("tm_test_b").await.expect("ensure b");
    let matched = drv.list_matching("tm_test_").await.expect("list");
    assert!(matched.contains(&"tm_test_a".to_string()));
    let dropped = drv.drop_matching("tm_test_").await.expect("drop");
    assert!(dropped.len() >= 2);
}

#[tokio::test]
#[ignore = "requires docker"]
async fn snapshot_via_template() {
    let (_c, cfg) = start_postgres().await;
    let drv = PostgresDriver::connect(&cfg).await.expect("connect");
    drv.ensure_db("tm_src").await.expect("ensure src");
    let pool = sqlx::PgPool::connect(&format!(
        "postgres://{}:@{}:{}/tm_src",
        cfg.user, cfg.host, cfg.port
    ))
    .await
    .expect("pool");
    sqlx::query("CREATE TABLE foo (id INT PRIMARY KEY, v TEXT)")
        .execute(&pool)
        .await
        .unwrap();
    sqlx::query("INSERT INTO foo VALUES (1, 'hello')")
        .execute(&pool)
        .await
        .unwrap();
    pool.close().await;
    drv.snapshot_create("tm_src", "tm_tmpl")
        .await
        .expect("snapshot create");
    drv.snapshot_restore("tm_tmpl", "tm_dst")
        .await
        .expect("snapshot restore");
    let dst_pool = sqlx::PgPool::connect(&format!(
        "postgres://{}:@{}:{}/tm_dst",
        cfg.user, cfg.host, cfg.port
    ))
    .await
    .unwrap();
    let row: (i32, String) = sqlx::query_as("SELECT id, v FROM foo WHERE id = 1")
        .fetch_one(&dst_pool)
        .await
        .expect("row");
    assert_eq!(row, (1, "hello".to_string()));
}
