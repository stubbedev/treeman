//! Live MySQL integration tests against an existing instance.
//!
//! Set `TREEMAN_TEST_MYSQL_URL` to a `mysql://user:pass@host:port/`
//! URL pointing at a database where the user can `CREATE DATABASE`.
//! Each test scopes itself to a fresh `tm_it_<rand>_*` database and
//! tears it down on completion, so it never collides with other
//! tenants on the same host.
//!
//! `cargo test --features integration -p treeman-db --test
//! integration_mysql -- --include-ignored`

#![cfg(feature = "integration")]

use treeman_core::config::MysqlConn;
use treeman_core::dburl;
use treeman_db::DbDriver;
use treeman_db::mysql::MysqlDriver;

fn env_cfg() -> Option<(MysqlConn, String)> {
    let url = std::env::var("TREEMAN_TEST_MYSQL_URL").ok()?;
    let parsed = dburl::parse(&url).ok()?;
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
    Some((cfg, stem))
}

#[tokio::test]
#[ignore = "set TREEMAN_TEST_MYSQL_URL"]
async fn ensure_and_drop_db() {
    let Some((cfg, stem)) = env_cfg() else {
        eprintln!("TREEMAN_TEST_MYSQL_URL not set — skipping");
        return;
    };
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
#[ignore = "set TREEMAN_TEST_MYSQL_URL"]
async fn snapshot_roundtrip() {
    let Some((cfg, stem)) = env_cfg() else {
        eprintln!("TREEMAN_TEST_MYSQL_URL not set — skipping");
        return;
    };
    let src = format!("{stem}_src");
    let tmpl = format!("{stem}_tmpl");
    let dst = format!("{stem}_dst");
    let drv = MysqlDriver::connect(&cfg).await.expect("connect");
    // Best-effort cleanup before/after.
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
