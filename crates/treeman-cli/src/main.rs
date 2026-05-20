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
    /// Worktree registration + listing.
    #[command(subcommand)]
    Worktree(WorktreeCmd),
    /// Run a configured hook phase against the worktree containing the cwd.
    #[command(subcommand)]
    Hook(HookCmd),
}

#[derive(Subcommand, Debug)]
enum WorktreeCmd {
    /// Register a worktree path (computes slug, inserts repos+worktrees rows).
    Register {
        #[arg(default_value = ".")]
        path: PathBuf,
        #[arg(long)]
        branch: Option<String>,
        #[arg(long)]
        repo: Option<PathBuf>,
    },
    /// List active worktrees.
    List,
    /// Mark a worktree deleted.
    Unregister {
        #[arg(default_value = ".")]
        path: PathBuf,
    },
}

#[derive(Subcommand, Debug)]
enum HookCmd {
    /// Run a hook phase (precreate|postcreate|predelete|postdelete) using
    /// the config of the worktree containing the cwd.
    Run {
        phase: String,
        #[arg(long)]
        worktree: Option<PathBuf>,
    },
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
        Cmd::Worktree(WorktreeCmd::Register { path, branch, repo }) => {
            worktree_register(path, branch, repo).await
        }
        Cmd::Worktree(WorktreeCmd::List) => worktree_list().await,
        Cmd::Worktree(WorktreeCmd::Unregister { path }) => worktree_unregister(path).await,
        Cmd::Hook(HookCmd::Run { phase, worktree }) => hook_run(phase, worktree).await,
    }
}

async fn worktree_register(
    path: PathBuf,
    branch: Option<String>,
    repo: Option<PathBuf>,
) -> Result<()> {
    let path = path.canonicalize().with_context(|| format!("canonicalize {}", path.display()))?;
    let repo_root = match repo {
        Some(r) => r.canonicalize()?,
        None => discover_repo_root(&path).context("could not find repo root for worktree")?,
    };
    let repo_name = repo_root.file_name().and_then(|s| s.to_str()).unwrap_or("repo").to_string();
    let branch = branch.or_else(|| detect_branch(&path));
    let slug = treeman_core::slug_for(&path, branch.as_deref());

    let pool = open_pool().await?;
    let repo_id = treeman_store::ensure_repo(&pool, &repo_root, &repo_name).await?;
    let wt_id = treeman_store::ensure_worktree(&pool, repo_id, &path, &slug.value, branch.as_deref()).await?;
    println!("worktree #{} slug={} repo=#{} ({})", wt_id, slug.value, repo_id, repo_root.display());
    Ok(())
}

async fn worktree_list() -> Result<()> {
    let pool = open_pool().await?;
    let rows = treeman_store::hook_runs::list_worktrees(&pool).await?;
    if rows.is_empty() {
        println!("(no worktrees registered)");
        return Ok(());
    }
    println!("{:<4} {:<24} {:<24} {}", "ID", "SLUG", "BRANCH", "PATH");
    for r in rows {
        println!("{:<4} {:<24} {:<24} {}",
            r.id, r.slug, r.branch.as_deref().unwrap_or("-"), r.path);
    }
    Ok(())
}

async fn worktree_unregister(path: PathBuf) -> Result<()> {
    let path = path.canonicalize().with_context(|| format!("canonicalize {}", path.display()))?;
    let pool = open_pool().await?;
    let wt = treeman_store::hook_runs::find_worktree_by_path(&pool, &path.to_string_lossy()).await?
        .with_context(|| format!("worktree not registered: {}", path.display()))?;
    treeman_store::mark_worktree_deleted(&pool, wt.id).await?;
    println!("unregistered worktree #{} ({})", wt.id, wt.path);
    Ok(())
}

async fn hook_run(phase: String, worktree: Option<PathBuf>) -> Result<()> {
    let wt_path = match worktree {
        Some(p) => p.canonicalize()?,
        None => std::env::current_dir()?,
    };
    let repo_root = discover_repo_root(&wt_path)
        .context("could not find repo root containing worktree")?;
    let cfg = treeman_core::config::load_layered(Some(&repo_root))?;
    let steps = match phase.as_str() {
        "precreate"  => &cfg.hooks.precreate,
        "postcreate" => &cfg.hooks.postcreate,
        "predelete"  => &cfg.hooks.predelete,
        "postdelete" => &cfg.hooks.postdelete,
        other => bail!("unknown hook phase: {other}"),
    };
    let branch = detect_branch(&wt_path);
    let slug = treeman_core::slug_for(&wt_path, branch.as_deref());

    let pool = open_pool().await?;
    let repo_name = repo_root.file_name().and_then(|s| s.to_str()).unwrap_or("repo");
    let repo_id = treeman_store::ensure_repo(&pool, &repo_root, repo_name).await?;
    let wt_id = treeman_store::ensure_worktree(&pool, repo_id, &wt_path, &slug.value, branch.as_deref()).await?;
    let run_id = treeman_store::hook_runs::start_hook_run(&pool, wt_id, &phase).await?;

    let start = std::time::Instant::now();
    let outcome = treeman_core::hooks::run_hooks(steps, &repo_root, &wt_path, &slug.value).await?;
    let duration_ms = start.elapsed().as_millis() as i64;
    let mut stdout = String::new();
    let mut stderr = String::new();
    for (i, s) in outcome.steps.iter().enumerate() {
        let kind = if s.background { "background" } else { "foreground" };
        stdout.push_str(&format!("--- step {i} ({kind}) ---\n{}\n", s.stdout_tail));
        if !s.stderr_tail.is_empty() {
            stderr.push_str(&format!("--- step {i} stderr ---\n{}\n", s.stderr_tail));
        }
        let payload = serde_json::json!({
            "command": s.command, "exit_code": s.exit_code, "background": s.background,
            "stdout_tail": s.stdout_tail, "stderr_tail": s.stderr_tail,
        }).to_string();
        let level = if s.exit_code == 0 { "info" } else { "error" };
        treeman_store::write_event(
            &pool, level, "hook_step", Some(&s.command),
            Some(repo_id), Some(wt_id), Some(&phase), None, &payload,
        ).await?;
    }
    treeman_store::hook_runs::finish_hook_run(&pool, run_id, outcome.aggregate_exit_code, &stdout, &stderr).await?;
    let summary = serde_json::json!({
        "run_id": run_id, "exit_code": outcome.aggregate_exit_code, "step_count": outcome.steps.len()
    }).to_string();
    let level = if outcome.aggregate_exit_code == 0 { "info" } else { "error" };
    treeman_store::write_event(
        &pool, level, "hook_run",
        Some(&format!("hook {} → exit {}", phase, outcome.aggregate_exit_code)),
        Some(repo_id), Some(wt_id), Some(&phase), Some(duration_ms), &summary,
    ).await?;

    println!("hook_run #{run_id} phase={phase} exit={}", outcome.aggregate_exit_code);
    if outcome.aggregate_exit_code != 0 {
        std::process::exit(outcome.aggregate_exit_code);
    }
    Ok(())
}

fn detect_branch(worktree: &std::path::Path) -> Option<String> {
    let head = worktree.join(".git/HEAD");
    let head_path = if head.is_file() {
        head
    } else {
        // Linked worktree: .git is a file pointing to gitdir.
        let raw = std::fs::read_to_string(worktree.join(".git")).ok()?;
        let gitdir = raw.trim_start_matches("gitdir:").trim();
        std::path::PathBuf::from(gitdir).join("HEAD")
    };
    let raw = std::fs::read_to_string(&head_path).ok()?;
    raw.trim().strip_prefix("ref: refs/heads/").map(|s| s.to_string())
}

fn discover_repo_root(start: &std::path::Path) -> Option<PathBuf> {
    let mut dir = start;
    loop {
        let dot_git = dir.join(".git");
        if dot_git.is_dir() {
            return Some(dir.to_path_buf());
        }
        if dot_git.is_file() {
            // Linked worktree: .git points to a gitdir under main repo.
            let raw = std::fs::read_to_string(&dot_git).ok()?;
            let gd = raw.trim_start_matches("gitdir:").trim();
            // commondir file inside the gitdir tells us the main repo's
            // git dir; we want its parent (the working tree).
            let common = std::path::PathBuf::from(gd).join("commondir");
            if let Ok(rel) = std::fs::read_to_string(&common) {
                let gitdir = std::path::PathBuf::from(gd);
                let common_dir = gitdir.join(rel.trim());
                return common_dir.canonicalize().ok().and_then(|p| p.parent().map(|p| p.to_path_buf()));
            }
            return Some(dir.to_path_buf());
        }
        dir = dir.parent()?;
    }
}

async fn open_pool() -> Result<sqlx::SqlitePool> {
    let p = treeman_store::default_db_path()?;
    treeman_store::open(&p).await
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
