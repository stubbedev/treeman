//! MongoDB driver. Snapshot via aggregation `$out` per collection +
//! manual index copy. No `mongodump`/`mongorestore` shell-out.

use anyhow::{Context, Result};
use async_trait::async_trait;
use futures_util::TryStreamExt;
use mongodb::bson::{Document, doc};
use mongodb::options::IndexOptions;
use mongodb::{Client, IndexModel};
use tracing::debug;
use treeman_core::config::MongoConn;

use crate::{DbDriver, Engine, Namespace};

pub struct MongoDriver {
    client: Client,
}

impl MongoDriver {
    pub async fn connect(cfg: &MongoConn) -> Result<Self> {
        let client = Client::with_uri_str(&cfg.uri)
            .await
            .context("mongo connect")?;
        Ok(Self { client })
    }

    pub async fn snapshot_create(&self, source: &str, template: &str) -> Result<()> {
        let src = self.client.database(source);
        let dst_name = template.to_string();
        // Drop any prior template.
        self.client.database(&dst_name).drop().await.ok();

        let collections = src.list_collection_names().await?;
        for coll_name in &collections {
            let coll = src.collection::<Document>(coll_name);
            // $out into template DB.
            let pipeline = vec![doc! {
                "$out": { "db": &dst_name, "coll": coll_name }
            }];
            let mut cursor = coll
                .aggregate(pipeline)
                .await
                .with_context(|| format!("$out for {coll_name}"))?;
            // Drain (some MongoDB versions return a cursor even for $out).
            while cursor.try_next().await?.is_some() {}

            // Copy indexes.
            let mut idx_cur = coll
                .list_indexes()
                .await
                .with_context(|| format!("list_indexes {coll_name}"))?;
            let mut models = Vec::new();
            while let Some(idx) = idx_cur.try_next().await? {
                if idx.keys.get_str("_id_").is_ok() {
                    continue;
                }
                // _id index always exists; reading model fields is enough.
                let keys = idx.keys.clone();
                let mut opts = IndexOptions::default();
                if let Some(o) = idx.options {
                    opts = o;
                }
                models.push(IndexModel::builder().keys(keys).options(opts).build());
            }
            if !models.is_empty() {
                self.client
                    .database(&dst_name)
                    .collection::<Document>(coll_name)
                    .create_indexes(models)
                    .await
                    .with_context(|| format!("create_indexes on template/{coll_name}"))?;
            }
        }
        debug!(source, template, "mongo snapshot create");
        Ok(())
    }

    pub async fn snapshot_restore(&self, template: &str, target: &str) -> Result<()> {
        self.client.database(target).drop().await.ok();
        self.snapshot_create(template, target).await
    }
}

#[async_trait]
impl DbDriver for MongoDriver {
    fn engine(&self) -> Engine {
        Engine::Mongodb
    }

    async fn engine_version(&self) -> Result<String> {
        let admin = self.client.database("admin");
        let info = admin.run_command(doc! {"buildInfo": 1}).await?;
        Ok(info.get_str("version").unwrap_or("unknown").to_string())
    }

    async fn ensure_db(&self, _name: &str) -> Result<()> {
        // Mongo auto-creates on first write.
        Ok(())
    }

    async fn drop_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let names: Vec<String> = self
            .client
            .list_database_names()
            .await?
            .into_iter()
            .filter(|n| n.starts_with(prefix))
            .collect();
        for n in &names {
            self.client
                .database(n)
                .drop()
                .await
                .with_context(|| format!("dropDatabase {n}"))?;
        }
        Ok(names)
    }

    async fn list_matching(&self, prefix: &str) -> Result<Vec<String>> {
        let all = self.client.list_database_names().await?;
        Ok(all.into_iter().filter(|n| n.starts_with(prefix)).collect())
    }

    async fn flush_namespace(&self, ns: &Namespace) -> Result<()> {
        match ns {
            Namespace::Database(name) => {
                self.client.database(name).drop().await?;
                Ok(())
            }
            _ => anyhow::bail!("mongo driver only supports Namespace::Database"),
        }
    }
}
