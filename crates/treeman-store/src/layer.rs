//! `tracing_subscriber::Layer` that writes each emitted event into the
//! SQLite `events` table. Designed to coexist with `fmt::Layer` so logs
//! still appear on stderr.

use std::collections::HashMap;
use std::sync::mpsc;
use std::thread;

use sqlx::SqlitePool;
use tracing::field::{Field, Visit};
use tracing::{Event, Subscriber};
use tracing_subscriber::Layer;
use tracing_subscriber::layer::Context;
use tracing_subscriber::registry::LookupSpan;

#[derive(Debug)]
pub struct EventInsert {
    pub level: &'static str,
    pub event_type: String,
    pub message: Option<String>,
    pub repo_id: Option<i64>,
    pub worktree_id: Option<i64>,
    pub phase: Option<String>,
    pub duration_ms: Option<i64>,
    pub payload_json: String,
}

/// Spawn a background writer task. Events are sent through an mpsc
/// channel; the writer batches inserts so the hot path in the tracing
/// layer is a single channel send.
pub struct SqliteLayer {
    sender: mpsc::SyncSender<EventInsert>,
}

impl SqliteLayer {
    pub fn new(pool: SqlitePool) -> Self {
        // Tracing's drop-on-error doesn't deadlock the writer. Bound the
        // queue so a runaway logger can't OOM us.
        let (tx, rx) = mpsc::sync_channel::<EventInsert>(4096);
        thread::Builder::new()
            .name("treeman-store-writer".into())
            .spawn(move || writer_loop(pool, rx))
            .expect("spawn writer");
        Self { sender: tx }
    }
}

fn writer_loop(pool: SqlitePool, rx: mpsc::Receiver<EventInsert>) {
    // Use a dedicated single-threaded tokio runtime so the writer can use
    // sqlx's async API.
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .expect("build runtime");
    rt.block_on(async move {
        while let Ok(ev) = rx.recv() {
            let now = chrono::Utc::now().timestamp_millis();
            let _ = sqlx::query(
                "INSERT INTO events(ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms)
                 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
            )
            .bind(now)
            .bind(ev.level)
            .bind(ev.repo_id)
            .bind(ev.worktree_id)
            .bind(ev.event_type)
            .bind(ev.phase)
            .bind(ev.message)
            .bind(ev.payload_json)
            .bind(ev.duration_ms)
            .execute(&pool)
            .await;
        }
    });
}

impl<S> Layer<S> for SqliteLayer
where
    S: Subscriber + for<'a> LookupSpan<'a>,
{
    fn on_event(&self, event: &Event<'_>, _ctx: Context<'_, S>) {
        let mut v = FieldVisitor::default();
        event.record(&mut v);
        let event_type = v
            .fields
            .get("event_type")
            .cloned()
            .unwrap_or_else(|| event.metadata().target().to_string());
        let phase = v.fields.get("phase").cloned();
        let repo_id = v.fields.get("repo_id").and_then(|s| s.parse().ok());
        let worktree_id = v.fields.get("worktree_id").and_then(|s| s.parse().ok());
        let duration_ms = v.fields.get("duration_ms").and_then(|s| s.parse().ok());
        let message = v.fields.remove("message");
        // Anything that wasn't a structured field we recognize goes into
        // the payload.
        for k in ["event_type", "phase", "repo_id", "worktree_id", "duration_ms"] {
            v.fields.remove(k);
        }
        let payload_json = serde_json::to_string(&v.fields).unwrap_or_else(|_| "{}".into());

        let level = match *event.metadata().level() {
            tracing::Level::TRACE => "debug",
            tracing::Level::DEBUG => "debug",
            tracing::Level::INFO => "info",
            tracing::Level::WARN => "warn",
            tracing::Level::ERROR => "error",
        };
        let _ = self.sender.try_send(EventInsert {
            level,
            event_type,
            message,
            repo_id,
            worktree_id,
            phase,
            duration_ms,
            payload_json,
        });
    }
}

#[derive(Default)]
struct FieldVisitor {
    fields: HashMap<String, String>,
}

impl Visit for FieldVisitor {
    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        self.fields.insert(field.name().to_string(), format!("{value:?}"));
    }
    fn record_str(&mut self, field: &Field, value: &str) {
        self.fields.insert(field.name().to_string(), value.to_string());
    }
    fn record_i64(&mut self, field: &Field, value: i64) {
        self.fields.insert(field.name().to_string(), value.to_string());
    }
    fn record_u64(&mut self, field: &Field, value: u64) {
        self.fields.insert(field.name().to_string(), value.to_string());
    }
    fn record_bool(&mut self, field: &Field, value: bool) {
        self.fields.insert(field.name().to_string(), value.to_string());
    }
}

/// Build a `tracing_subscriber` registry combining a console `fmt` layer
/// + the SQLite layer. The caller is expected to call this once at daemon
/// startup before any tracing macros fire.
pub fn init_subscriber(pool: SqlitePool) -> anyhow::Result<()> {
    use tracing_subscriber::layer::SubscriberExt;
    use tracing_subscriber::util::SubscriberInitExt;
    let env_filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::registry()
        .with(env_filter)
        .with(tracing_subscriber::fmt::layer().with_target(false))
        .with(SqliteLayer::new(pool))
        .try_init()
        .map_err(|e| anyhow::anyhow!("init tracing subscriber: {e}"))?;
    Ok(())
}
