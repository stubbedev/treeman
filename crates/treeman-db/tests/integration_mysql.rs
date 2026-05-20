//! Live MySQL integration tests.
//!
//! If `TREEMAN_TEST_MYSQL_URL` is set, runs against that instance.
//! Otherwise spins up a `testcontainers` MySQL container — the CI
//! path. Tests scope themselves to a unique `tm_it_<hash>_*` database
//! and drop their state on completion.
//!
//! `cargo test --features integration -p treeman-db --test
//! integration_mysql -- --include-ignored`

#![cfg(feature = "integration")]

use testcontainers_modules::mysql::Mysql;
use testcontainers_modules::testcontainers::ContainerAsync;
use testcontainers_modules::testcontainers::runners::AsyncRunner;
use treeman_core::config::MysqlConn;
use treeman_core::dburl;
use treeman_db::DbDriver;
use treeman_db::mysql::MysqlDriver;

#[allow(dead_code, clippy::large_enum_variant)]
enum Backing {
    Env,
    // Held purely to keep the container alive for the test duration.
    Container(ContainerAsync<Mysql>),
}

async fn backing() -> (Backing, MysqlConn, String) {
    if let Ok(url) = std::env::var("TREEMAN_TEST_MYSQL_URL") {
        let parsed = dburl::parse(&url).expect("parse TREEMAN_TEST_MYSQL_URL");
        let cfg = MysqlConn {
            host: parsed.host.clone().unwrap_or_else(|| "127.0.0.1".into()),
            port: parsed.port.unwrap_or(3306),
            user: parsed.user.clone().unwrap_or_else(|| "root".into()),
            password_env: None,
            pool_max: 4,
        };
        let stem = format!(
            "tm_it_{}",
            &blake3::hash(url.as_bytes()).to_hex().to_string()[..10]
        );
        return (Backing::Env, cfg, stem);
    }
    let container = Mysql::default().start().await.expect("start mysql");
    let port = container.get_host_port_ipv4(3306).await.expect("host port");
    let cfg = MysqlConn {
        host: "127.0.0.1".into(),
        port,
        user: "root".into(),
        password_env: None,
        password: None,
        pool_max: 4,
    };
    let stem = format!("tm_it_{port}");
    (Backing::Container(container), cfg, stem)
}

#[tokio::test]
#[ignore = "live MySQL — env or docker"]
async fn ensure_and_drop_db() {
    let (_b, cfg, stem) = backing().await;
    let drv = MysqlDriver::connect(&cfg).await.expect("connect");
    let a = format!("{stem}_a");
    let b = format!("{stem}_b");
    drv.ensure_db(&a).await.expect("ensure a");
    drv.ensure_db(&b).await.expect("ensure b");
    let matched = drv.list_matching(&stem).await.expect("list");
    assert!(matched.contains(&a));
    assert!(matched.contains(&b));
    let dropped = drv.drop_matching(&stem).await.expect("drop");
    assert!(dropped.contains(&a));
    assert!(dropped.contains(&b));
    let after = drv.list_matching(&stem).await.expect("list");
    assert!(after.is_empty());
}

#[tokio::test]
#[ignore = "live MySQL — env or docker"]
async fn snapshot_roundtrip() {
    let (_b, cfg, stem) = backing().await;
    let drv = MysqlDriver::connect(&cfg).await.expect("connect");
    let src = format!("{stem}_src");
    let tmpl = format!("{stem}_tmpl");
    let dst = format!("{stem}_dst");
    let _ = drv.drop_matching(&stem).await;
    drv.ensure_db(&src).await.expect("ensure src");
    let pool = sqlx::MySqlPool::connect(&format!(
        "mysql://{}:@{}:{}/{src}",
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
    drv.snapshot_create(&src, &tmpl)
        .await
        .expect("snapshot create");
    drv.snapshot_restore(&tmpl, &dst)
        .await
        .expect("snapshot restore");
    let dst_pool = sqlx::MySqlPool::connect(&format!(
        "mysql://{}:@{}:{}/{dst}",
        cfg.user, cfg.host, cfg.port
    ))
    .await
    .unwrap();
    let row: (i32, String) = sqlx::query_as("SELECT id, v FROM foo WHERE id = 1")
        .fetch_one(&dst_pool)
        .await
        .expect("row in cloned db");
    assert_eq!(row, (1, "hello".to_string()));
    let _ = drv.drop_matching(&stem).await;
}
