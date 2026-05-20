//! Live PostgreSQL integration tests. Uses `TREEMAN_TEST_POSTGRES_URL`
//! when set, otherwise spins up `testcontainers` Postgres. Tests
//! scope to `tm_it_<hash>_*`.

#![cfg(feature = "integration")]

use testcontainers_modules::postgres::Postgres;
use testcontainers_modules::testcontainers::ContainerAsync;
use testcontainers_modules::testcontainers::runners::AsyncRunner;
use treeman_core::config::PostgresConn;
use treeman_core::dburl;
use treeman_db::DbDriver;
use treeman_db::postgres::PostgresDriver;

#[allow(dead_code, clippy::large_enum_variant)]
enum Backing {
    Env,
    Container(ContainerAsync<Postgres>),
}

async fn backing() -> (Backing, PostgresConn, String) {
    if let Ok(url) = std::env::var("TREEMAN_TEST_POSTGRES_URL") {
        let parsed = dburl::parse(&url).expect("parse TREEMAN_TEST_POSTGRES_URL");
        let cfg = PostgresConn {
            host: parsed.host.clone().unwrap_or_else(|| "127.0.0.1".into()),
            port: parsed.port.unwrap_or(5432),
            user: parsed.user.clone().unwrap_or_else(|| "postgres".into()),
            password_env: None,
            pool_max: 4,
        };
        let stem = format!(
            "tm_it_{}",
            &blake3::hash(url.as_bytes()).to_hex().to_string()[..10]
        );
        return (Backing::Env, cfg, stem);
    }
    let container = Postgres::default().start().await.expect("start pg");
    let port = container.get_host_port_ipv4(5432).await.expect("port");
    let cfg = PostgresConn {
        host: "127.0.0.1".into(),
        port,
        user: "postgres".into(),
        password_env: None,
        pool_max: 4,
    };
    let stem = format!("tm_it_{port}");
    (Backing::Container(container), cfg, stem)
}

#[tokio::test]
#[ignore = "live Postgres — env or docker"]
async fn ensure_and_drop_db() {
    let (_b, cfg, stem) = backing().await;
    let drv = PostgresDriver::connect(&cfg).await.expect("connect");
    let a = format!("{stem}_a");
    let b = format!("{stem}_b");
    drv.ensure_db(&a).await.expect("ensure a");
    drv.ensure_db(&b).await.expect("ensure b");
    let matched = drv.list_matching(&stem).await.expect("list");
    assert!(matched.contains(&a));
    let dropped = drv.drop_matching(&stem).await.expect("drop");
    assert!(dropped.contains(&a));
    assert!(dropped.contains(&b));
}

#[tokio::test]
#[ignore = "live Postgres — env or docker"]
async fn snapshot_via_template() {
    let (_b, cfg, stem) = backing().await;
    let drv = PostgresDriver::connect(&cfg).await.expect("connect");
    let src = format!("{stem}_src");
    let tmpl = format!("{stem}_tmpl");
    let dst = format!("{stem}_dst");
    let _ = drv.drop_matching(&stem).await;
    drv.ensure_db(&src).await.expect("ensure src");
    let pool = sqlx::PgPool::connect(&format!(
        "postgres://{}:@{}:{}/{src}",
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
    drv.snapshot_create(&src, &tmpl)
        .await
        .expect("snapshot create");
    drv.snapshot_restore(&tmpl, &dst)
        .await
        .expect("snapshot restore");
    let dst_pool = sqlx::PgPool::connect(&format!(
        "postgres://{}:@{}:{}/{dst}",
        cfg.user, cfg.host, cfg.port
    ))
    .await
    .unwrap();
    let row: (i32, String) = sqlx::query_as("SELECT id, v FROM foo WHERE id = 1")
        .fetch_one(&dst_pool)
        .await
        .expect("row");
    assert_eq!(row, (1, "hello".to_string()));
    let _ = drv.drop_matching(&stem).await;
}
