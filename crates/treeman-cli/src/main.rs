//! `treeman` — thin CLI client for `treemand` plus a few local commands
//! (`config validate`, `slug`, `schema dump`) that don't require the daemon.

use std::path::PathBuf;

use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand};
use directories::ProjectDirs;
use jsonrpsee::core::client::ClientT;
use jsonrpsee::http_client::HttpClientBuilder;
use jsonrpsee::rpc_params;
use treeman_proto::{SOCKET_BASENAME, SOCKET_ENV, StatusResponse};
use treeman_store::query::{EventFilter, query_events, tail_events};

#[derive(Parser, Debug)]
#[command(name = "treeman", version, about = "Treeman CLI", long_about = None)]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand, Debug)]
enum Cmd {
    /// Print daemon version, pid, uptime.
    Status,
    /// Slug derivation utility (local — no daemon needed).
    Slug(SlugArgs),
    /// Configuration helpers.
    #[command(subcommand)]
    Config(ConfigCmd),
    /// Emit the JSON Schema for `.treeman.yaml`.
    #[command(subcommand)]
    Schema(SchemaCmd),
    /// Inspect the SQLite event log.
    #[command(subcommand)]
    Logs(LogsCmd),
}

#[derive(Subcommand, Debug)]
enum LogsCmd {
    /// Tail recent events (newest last). With `--follow`, poll for new
    /// rows until interrupted.
    Tail {
        #[arg(short, long)]
        follow: bool,
        #[arg(long, default_value_t = 50)]
        limit: i64,
        #[arg(long)]
        level: Option<String>,
        #[arg(long)]
        event_type: Option<String>,
        #[arg(long)]
        worktree: Option<i64>,
    },
    /// Grep recent events.
    Grep {
        pattern: String,
        #[arg(long, default_value_t = 200)]
        limit: i64,
        #[arg(long)]
        level: Option<String>,
        #[arg(long)]
        event_type: Option<String>,
    },
}

#[derive(clap::Args, Debug)]
struct SlugArgs {
    /// Worktree directory.
    #[arg(default_value = ".")]
    path: PathBuf,
    /// Optional branch name (overrides path-based ticket extraction).
    #[arg(long)]
    branch: Option<String>,
    /// Print the redis db indices derived from the slug.
    #[arg(long)]
    redis: bool,
}

#[derive(Subcommand, Debug)]
enum ConfigCmd {
    /// Load global + per-repo config and report errors.
    Validate {
        /// Repo root to load `.treeman.yaml` from (default: cwd or git root).
        #[arg(long)]
        repo: Option<PathBuf>,
    },
    /// Print the loaded, merged config as YAML.
    Show {
        #[arg(long)]
        repo: Option<PathBuf>,
    },
}

#[derive(Subcommand, Debug)]
enum SchemaCmd {
    /// Print the JSON Schema for the full config to stdout.
    Dump,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.cmd {
        Cmd::Status => status().await,
        Cmd::Slug(args) => slug(args),
        Cmd::Config(ConfigCmd::Validate { repo }) => config_validate(repo),
        Cmd::Config(ConfigCmd::Show { repo }) => config_show(repo),
        Cmd::Schema(SchemaCmd::Dump) => schema_dump(),
        Cmd::Logs(LogsCmd::Tail { follow, limit, level, event_type, worktree }) => {
            logs_tail(follow, limit, level, event_type, worktree).await
        }
        Cmd::Logs(LogsCmd::Grep { pattern, limit, level, event_type }) => {
            logs_grep(pattern, limit, level, event_type).await
        }
    }
}

async fn logs_tail(
    follow: bool,
    limit: i64,
    level: Option<String>,
    event_type: Option<String>,
    worktree: Option<i64>,
) -> Result<()> {
    let db_path = treeman_store::default_db_path()?;
    let pool = treeman_store::open(&db_path).await?;
    let filter = EventFilter {
        limit: Some(limit), level: level.clone(), event_type: event_type.clone(),
        worktree_id: worktree, ..Default::default()
    };
    let mut rows = query_events(&pool, &filter).await?;
    rows.reverse(); // newest last
    let mut last_id = rows.iter().map(|r| r.id).max().unwrap_or(0);
    for r in &rows { print_event(r); }
    if !follow {
        return Ok(());
    }
    loop {
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
        let next = tail_events(&pool, last_id).await?;
        for r in &next {
            if let Some(ref l) = level   { if &r.level != l { continue; } }
            if let Some(ref t) = event_type { if &r.event_type != t { continue; } }
            if let Some(w) = worktree { if r.worktree_id != Some(w) { continue; } }
            print_event(r);
            last_id = r.id;
        }
    }
}

async fn logs_grep(
    pattern: String,
    limit: i64,
    level: Option<String>,
    event_type: Option<String>,
) -> Result<()> {
    let db_path = treeman_store::default_db_path()?;
    let pool = treeman_store::open(&db_path).await?;
    let filter = EventFilter {
        limit: Some(limit), level, event_type, grep: Some(pattern),
        ..Default::default()
    };
    let mut rows = query_events(&pool, &filter).await?;
    rows.reverse();
    for r in &rows { print_event(r); }
    Ok(())
}

fn print_event(r: &treeman_store::EventRow) {
    let ts = chrono::DateTime::from_timestamp_millis(r.ts)
        .map(|t| t.format("%Y-%m-%d %H:%M:%S%.3f").to_string())
        .unwrap_or_else(|| r.ts.to_string());
    let msg = r.message.as_deref().unwrap_or("");
    println!("{ts} {:5} {} {}", r.level.to_uppercase(), r.event_type, msg);
}

async fn status() -> Result<()> {
    let addr = resolve_addr()?;
    let url = format!("http://{addr}");
    let client = HttpClientBuilder::default().build(&url)
        .with_context(|| format!("connect {url}"))?;
    let resp: StatusResponse = client
        .request("status", rpc_params![])
        .await
        .context("status rpc")?;
    println!("daemon_version: {}", resp.daemon_version);
    println!("protocol:       v{}", resp.protocol_version);
    println!("pid:            {}", resp.pid);
    println!("started_at:     {}", resp.started_at_unix);
    println!("watchers:       {}", resp.watcher_count);
    Ok(())
}

fn slug(args: SlugArgs) -> Result<()> {
    let s = treeman_core::slug_for(&args.path, args.branch.as_deref());
    println!("{}", s.value);
    if args.redis {
        let (q, c) = s.redis_indices();
        println!("redis_queue={q}");
        println!("redis_cache={c}");
    }
    Ok(())
}

fn config_validate(repo: Option<PathBuf>) -> Result<()> {
    let repo = resolve_repo(repo)?;
    let cfg = treeman_core::config::load_layered(repo.as_deref())
        .context("load config")?;
    println!("ok: config loaded ({} databases configured)", cfg.databases.len());
    Ok(())
}

fn config_show(repo: Option<PathBuf>) -> Result<()> {
    let repo = resolve_repo(repo)?;
    let cfg = treeman_core::config::load_layered(repo.as_deref())?;
    let s = serde_yaml::to_string(&cfg)?;
    print!("{s}");
    Ok(())
}

fn schema_dump() -> Result<()> {
    let s = treeman_core::config::json_schema();
    println!("{}", serde_json::to_string_pretty(&s)?);
    Ok(())
}

fn resolve_repo(explicit: Option<PathBuf>) -> Result<Option<PathBuf>> {
    if let Some(p) = explicit {
        return Ok(Some(p));
    }
    // Walk up from cwd looking for .treeman.yaml or .git.
    let cwd = std::env::current_dir()?;
    let mut dir = cwd.as_path();
    loop {
        if dir.join(".treeman.yaml").exists() || dir.join(".git").exists() {
            return Ok(Some(dir.to_path_buf()));
        }
        match dir.parent() {
            Some(p) => dir = p,
            None => return Ok(None),
        }
    }
}

fn resolve_addr() -> Result<String> {
    let socket_path = resolve_socket_path()?;
    let addr_path = socket_path.with_extension("addr");
    let addr = std::fs::read_to_string(&addr_path).with_context(|| {
        format!(
            "could not read {} — is treemand running? (start with `treemand`)",
            addr_path.display()
        )
    })?;
    let trimmed = addr.trim().to_string();
    if trimmed.is_empty() {
        bail!("{} is empty", addr_path.display());
    }
    Ok(trimmed)
}

fn resolve_socket_path() -> Result<PathBuf> {
    if let Ok(s) = std::env::var(SOCKET_ENV) {
        return Ok(PathBuf::from(s));
    }
    if let Ok(rt) = std::env::var("XDG_RUNTIME_DIR") {
        if !rt.is_empty() {
            return Ok(PathBuf::from(rt).join(SOCKET_BASENAME));
        }
    }
    let dirs = ProjectDirs::from("", "", "treeman")
        .context("could not resolve treeman state directory")?;
    Ok(dirs.data_local_dir().join(SOCKET_BASENAME))
}
