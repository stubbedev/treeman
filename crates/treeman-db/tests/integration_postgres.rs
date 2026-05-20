//! Live PostgreSQL integration tests against an existing instance.
//! Set `TREEMAN_TEST_POSTGRES_URL` to a `postgres://user:pass@host:port/`
//! URL. Tests scope to a fresh `tm_it_<rand>_*` database.

#![cfg(feature = "integration")]

use treeman_core::config::PostgresConn;
use treeman_core::dburl;
use treeman_db::DbDriver;
use treeman_db::postgres::PostgresDriver;

fn env_cfg() -> Option<(PostgresConn, String)> {
    let url = std::env::var("TREEMAN_TEST_POSTGRES_URL").ok()?;
    let parsed = dburl::parse(&url).ok()?;
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
    Some((cfg, stem))
}

#[tokio::test]
#[ignore = "set TREEMAN_TEST_POSTGRES_URL"]
async fn ensure_and_drop_db() {
    let Some((cfg, stem)) = env_cfg() else {
        eprintln!("TREEMAN_TEST_POSTGRES_URL not set — skipping");
        return;
    };
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
#[ignore = "set TREEMAN_TEST_POSTGRES_URL"]
async fn snapshot_via_template() {
    let Some((cfg, stem)) = env_cfg() else {
        eprintln!("TREEMAN_TEST_POSTGRES_URL not set — skipping");
        return;
    };
    let src = format!("{stem}_src");
    let tmpl = format!("{stem}_tmpl");
    let dst = format!("{stem}_dst");
    let drv = PostgresDriver::connect(&cfg).await.expect("connect");
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
