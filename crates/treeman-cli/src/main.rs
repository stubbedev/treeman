//! `treeman` CLI. Thin client for `treemand` plus local-only commands
//! (`config`, `slug`, `schema`, `init`).
//!
//! See `--help` for the full subcommand tree. `treeman completions
//! <shell>` emits a tab-completion script for bash/zsh/fish/elvish.

use std::path::{Path, PathBuf};

mod client;

use anyhow::{Context, Result, bail};
use clap::{ArgAction, CommandFactory, Parser, Subcommand, ValueEnum, ValueHint};
use clap_complete::Shell;
use is_terminal::IsTerminal;
use owo_colors::{OwoColorize, Style};
use treeman_proto::{Request, Response};
use treeman_store::query::{EventFilter, query_events, tail_events};

// ───────────────────────── value_enums ─────────────────────────

#[derive(Debug, Clone, Copy, ValueEnum)]
#[clap(rename_all = "lower")]
enum EngineArg {
    Mysql,
    Mariadb,
    Tidb,
    Postgres,
    Cockroach,
    #[clap(alias = "mongo")]
    Mongodb,
    Redis,
    #[clap(alias = "es")]
    Elasticsearch,
    Opensearch,
    Sqlite,
    Duckdb,
    Clickhouse,
    Meilisearch,
    Typesense,
    Qdrant,
    Weaviate,
    Milvus,
    Neo4j,
    Influxdb,
    Memcached,
    Rabbitmq,
    Nats,
    Etcd,
    Kafka,
    S3,
}

impl EngineArg {
    fn as_str(self) -> &'static str {
        match self {
            EngineArg::Mysql => "mysql",
            EngineArg::Mariadb => "mariadb",
            EngineArg::Tidb => "tidb",
            EngineArg::Postgres => "postgres",
            EngineArg::Cockroach => "cockroach",
            EngineArg::Mongodb => "mongodb",
            EngineArg::Redis => "redis",
            EngineArg::Elasticsearch => "elasticsearch",
            EngineArg::Opensearch => "opensearch",
            EngineArg::Sqlite => "sqlite",
            EngineArg::Duckdb => "duckdb",
            EngineArg::Clickhouse => "clickhouse",
            EngineArg::Meilisearch => "meilisearch",
            EngineArg::Typesense => "typesense",
            EngineArg::Qdrant => "qdrant",
            EngineArg::Weaviate => "weaviate",
            EngineArg::Milvus => "milvus",
            EngineArg::Neo4j => "neo4j",
            EngineArg::Influxdb => "influxdb",
            EngineArg::Memcached => "memcached",
            EngineArg::Rabbitmq => "rabbitmq",
            EngineArg::Nats => "nats",
            EngineArg::Etcd => "etcd",
            EngineArg::Kafka => "kafka",
            EngineArg::S3 => "s3",
        }
    }
}

#[derive(Debug, Clone, Copy, ValueEnum)]
#[clap(rename_all = "lower")]
enum PhaseArg {
    Precreate,
    Postcreate,
    Predelete,
    Postdelete,
}

impl PhaseArg {
    fn as_str(self) -> &'static str {
        match self {
            PhaseArg::Precreate => "precreate",
            PhaseArg::Postcreate => "postcreate",
            PhaseArg::Predelete => "predelete",
            PhaseArg::Postdelete => "postdelete",
        }
    }
}

#[derive(Debug, Clone, Copy, ValueEnum)]
#[clap(rename_all = "lower")]
enum LevelArg {
    Debug,
    Info,
    Warn,
    Error,
}
impl LevelArg {
    fn as_str(self) -> &'static str {
        match self {
            LevelArg::Debug => "debug",
            LevelArg::Info => "info",
            LevelArg::Warn => "warn",
            LevelArg::Error => "error",
        }
    }
}

// ───────────────────────── top-level ─────────────────────────

const LONG_ABOUT: &str = "\
Treeman — pure-wire per-worktree DB orchestrator.

Common flows:
  treeman init                       # generate .treeman.yaml from detected framework
  treeman daemon start               # ensure treemand running
  treeman wt create PROJ-1234        # git worktree add + DB scoping + prepare
  treeman watcher start              # daemon-managed migration file watcher
  treeman wt delete PROJ-1234        # predelete hook + DB teardown + git worktree remove

Tab completions:
  treeman completions zsh > _treeman    # then drop on fpath / source
  treeman completions bash > treeman.bash
";

#[derive(Parser, Debug)]
#[command(
    name = "treeman",
    version = concat!(env!("CARGO_PKG_VERSION")),
    about = "Per-worktree DB orchestrator with file watcher",
    long_about = LONG_ABOUT,
    arg_required_else_help = true,
    propagate_version = true,
)]
struct Cli {
    /// Decrease verbosity (errors only).
    #[arg(short, long, global = true, action = ArgAction::Count)]
    quiet: u8,
    /// Increase verbosity (-v info, -vv debug, -vvv trace).
    #[arg(short, long, global = true, action = ArgAction::Count, conflicts_with = "quiet")]
    verbose: u8,
    /// Disable colored output (also honored: NO_COLOR env var, non-TTY stdout).
    #[arg(long, global = true)]
    no_color: bool,

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
    #[command(subcommand, visible_alias = "log")]
    Logs(LogsCmd),
    /// Worktree lifecycle.
    #[command(subcommand, visible_alias = "wt")]
    Worktree(WorktreeCmd),
    /// Run a configured hook phase.
    #[command(subcommand)]
    Hook(HookCmd),
    /// Direct DB driver operations.
    #[command(subcommand)]
    Db(DbCmd),
    /// Migration framework detection.
    #[command(subcommand, visible_alias = "fw")]
    Frameworks(FrameworksCmd),
    /// Watch migration directories foreground (CLI-only; daemon-managed alt: `treeman watcher start`).
    Watch(WatchArgs),
    /// Snapshot catalog operations.
    #[command(subcommand, visible_alias = "snap")]
    Snapshot(SnapshotCmd),
    /// Full prepare: ensure → dump → migrate → snapshot → replicate (DB clones for the detected test framework).
    Prepare(PrepareArgs),
    /// Daemon lifecycle.
    #[command(subcommand)]
    Daemon(DaemonCmd),
    /// Watcher control (talks to the daemon).
    #[command(subcommand)]
    Watcher(WatcherCmd),
    /// Bootstrap .treeman.yaml from the detected framework.
    Init(InitArgs),
    /// Emit a shell completion script.
    Completions(CompletionsArgs),
    /// Emit a roff(7) man page for treeman to stdout.
    Manpage,
}

// ───────────────────────── subcommands ─────────────────────────

#[derive(Subcommand, Debug)]
enum DaemonCmd {
    /// Start the daemon (via systemctl/launchctl if installed, else spawn directly).
    Start,
    /// Stop the daemon (via systemctl/launchctl if installed, else send Shutdown RPC).
    Stop,
    /// Stop then start.
    Restart,
    /// Same as `treeman status`.
    Status,
    /// Install as a user service (systemd --user on Linux, LaunchAgent on macOS) and start it.
    Install,
    /// Stop the service and remove the user-service unit/plist.
    Uninstall,
}

#[derive(Subcommand, Debug)]
enum WatcherCmd {
    /// Tell the daemon to watch this (or a given) repo.
    Start {
        #[command(flatten)]
        common: RepoCommon,
    },
    /// Stop watching a repo.
    Stop {
        #[command(flatten)]
        common: RepoCommon,
    },
    /// List currently-watched repos.
    List,
    /// List linked worktrees currently being watched for a repo.
    Worktrees {
        #[command(flatten)]
        common: RepoCommon,
    },
}

#[derive(clap::Args, Debug)]
struct InitArgs {
    #[command(flatten)]
    repo: RepoCommon,
    /// Overwrite existing .treeman.yaml.
    #[arg(short, long)]
    force: bool,
}

#[derive(clap::Args, Debug)]
struct PrepareArgs {
    #[command(flatten)]
    worktree: WorktreeCommon,
    #[command(flatten)]
    repo: RepoCommon,
}

#[derive(Subcommand, Debug)]
enum SnapshotCmd {
    /// List recorded snapshots.
    #[command(visible_alias = "ls")]
    List {
        #[arg(short, long, value_enum)]
        engine: Option<EngineArg>,
    },
    /// Show a single snapshot by fingerprint (prefix match OK).
    Show { fingerprint: String },
    /// Run LRU GC.
    Gc {
        #[arg(short = 'k', long, default_value_t = 5)]
        keep_per_source: u32,
        #[arg(long, default_value_t = 30)]
        max_age_days: u32,
        #[arg(long, default_value_t = 50)]
        max_total_gb: u32,
    },
}

#[derive(Subcommand, Debug)]
enum FrameworksCmd {
    /// List built-in + YAML-declared frameworks; print which ones the repo matches.
    Detect {
        #[command(flatten)]
        repo: RepoCommon,
    },
}

#[derive(clap::Args, Debug)]
struct WatchArgs {
    #[command(flatten)]
    repo: RepoCommon,
}

#[derive(Subcommand, Debug)]
enum DbCmd {
    /// Drop every database matching a prefix.
    #[command(visible_alias = "rm")]
    Drop {
        #[arg(short, long, value_enum)]
        engine: EngineArg,
        #[arg(short, long)]
        prefix: String,
        #[command(flatten)]
        repo: RepoCommon,
    },
    /// FLUSHDB on a redis db index, or drop+recreate matching DBs.
    Flush {
        #[arg(short, long, value_enum)]
        engine: EngineArg,
        /// Redis db index (0..15). Requires --engine redis.
        #[arg(long, conflicts_with = "prefix")]
        db: Option<u8>,
        /// Name prefix (mysql/pg/mongo).
        #[arg(short, long, conflicts_with = "db")]
        prefix: Option<String>,
        #[command(flatten)]
        repo: RepoCommon,
    },
    /// List databases/indices matching a prefix.
    #[command(visible_alias = "ls")]
    List {
        #[arg(short, long, value_enum)]
        engine: EngineArg,
        #[arg(short, long, default_value = "")]
        prefix: String,
        #[command(flatten)]
        repo: RepoCommon,
    },
}

#[derive(Subcommand, Debug)]
enum WorktreeCmd {
    /// Create a new worktree end-to-end.
    #[command(visible_alias = "new")]
    Create {
        /// Branch name. Created from --from if it doesn't exist.
        branch: String,
        /// Base branch for new-branch creation. Defaults to the repo's default branch.
        #[arg(short = 'b', long)]
        from: Option<String>,
        /// Override worktree path (default: <cfg.worktrees.root>/<branch>).
        #[arg(short, long, value_hint = ValueHint::DirPath)]
        path: Option<PathBuf>,
        #[command(flatten)]
        repo: RepoCommon,
        /// Skip postcreate hooks + prepare.
        #[arg(long)]
        skip_hooks: bool,
        /// Skip prepare even if hooks ran.
        #[arg(long)]
        skip_prepare: bool,
        /// Force foreground execution of postcreate hooks + prepare
        /// even when `worktrees.async_create` is enabled. Useful for
        /// CI / scripted flows where you want the exit code to
        /// reflect whether DB scaffolding succeeded.
        #[arg(long)]
        foreground: bool,
    },
    /// Delete a worktree end-to-end.
    #[command(visible_alias = "rm")]
    Delete {
        /// Worktree path OR branch name.
        target: String,
        #[command(flatten)]
        repo: RepoCommon,
        /// Force removal even on dirty worktree + predelete failure.
        #[arg(short, long)]
        force: bool,
        /// Force foreground execution of predelete + DB teardown +
        /// git remove. By default `worktrees.async_delete` (true)
        /// hands the work to the daemon and returns immediately;
        /// use this for CI / scripted flows where the exit code
        /// must reflect whether teardown succeeded.
        #[arg(long)]
        foreground: bool,
    },
    /// Register a worktree path (metadata only).
    Register {
        #[arg(default_value = ".", value_hint = ValueHint::DirPath)]
        path: PathBuf,
        #[arg(short = 'b', long)]
        branch: Option<String>,
        #[command(flatten)]
        repo: RepoCommon,
    },
    /// List active worktrees.
    #[command(visible_alias = "ls")]
    List,
    /// Mark a worktree deleted in SQLite (without touching git).
    Unregister {
        #[arg(default_value = ".", value_hint = ValueHint::DirPath)]
        path: PathBuf,
    },
    /// Hand the daemon a "rerun postcreate + prepare for this
    /// worktree" RPC. Useful when a previous create was interrupted
    /// or the worktree dir was recovered from an orphan repair.
    Finalize {
        #[arg(default_value = ".", value_hint = ValueHint::DirPath)]
        path: PathBuf,
        #[command(flatten)]
        repo: RepoCommon,
    },
}

#[derive(Subcommand, Debug)]
enum HookCmd {
    /// Run a hook phase using the cwd's repo config.
    Run {
        #[arg(value_enum)]
        phase: PhaseArg,
        #[command(flatten)]
        worktree: WorktreeCommon,
    },
}

#[derive(Subcommand, Debug)]
enum LogsCmd {
    /// Tail recent events (newest last). `-f` polls for new rows.
    Tail {
        #[arg(short, long)]
        follow: bool,
        #[arg(short = 'n', long, default_value_t = 50)]
        limit: i64,
        #[arg(short = 'l', long, value_enum)]
        level: Option<LevelArg>,
        #[arg(short = 't', long)]
        event_type: Option<String>,
        #[arg(short = 'w', long)]
        worktree: Option<i64>,
    },
    /// Grep recent events.
    Grep {
        pattern: String,
        #[arg(short = 'n', long, default_value_t = 200)]
        limit: i64,
        #[arg(short = 'l', long, value_enum)]
        level: Option<LevelArg>,
        #[arg(short = 't', long)]
        event_type: Option<String>,
    },
}

#[derive(clap::Args, Debug)]
struct SlugArgs {
    /// Worktree directory.
    #[arg(default_value = ".", value_hint = ValueHint::DirPath)]
    path: PathBuf,
    /// Optional branch name (overrides path-based ticket extraction).
    #[arg(short = 'b', long)]
    branch: Option<String>,
    /// Also print the redis db indices derived from the slug.
    #[arg(long)]
    redis: bool,
}

#[derive(Subcommand, Debug)]
enum ConfigCmd {
    /// Load global + per-repo config and report errors.
    Validate {
        #[command(flatten)]
        repo: RepoCommon,
    },
    /// Print the loaded, merged config as YAML.
    Show {
        #[command(flatten)]
        repo: RepoCommon,
        /// Include per-engine connection resolution with provenance.
        #[arg(long)]
        resolved: bool,
    },
}

#[derive(Subcommand, Debug)]
enum SchemaCmd {
    /// Print the JSON Schema for the full config to stdout.
    Dump,
    /// Write JSON Schemas to ~/.config/treeman/schemas/ and print the
    /// `yaml-language-server` modeline that activates completions in
    /// editors backed by an LSP (VS Code, Neovim, Helix, Zed, …).
    Install {
        /// Output directory (default: $XDG_CONFIG_HOME/treeman/schemas).
        #[arg(long, value_hint = ValueHint::DirPath)]
        dir: Option<PathBuf>,
    },
}

#[derive(clap::Args, Debug)]
struct CompletionsArgs {
    /// Target shell.
    #[arg(value_enum)]
    shell: Shell,
}

/// Common --repo flag with env-var fallback.
#[derive(clap::Args, Debug, Clone)]
struct RepoCommon {
    /// Repository root (default: walk up from cwd until .git / .treeman.yaml).
    #[arg(short = 'r', long, env = "TREEMAN_REPO", value_hint = ValueHint::DirPath)]
    repo: Option<PathBuf>,
}

/// Common --worktree flag with env-var fallback.
#[derive(clap::Args, Debug, Clone)]
struct WorktreeCommon {
    /// Worktree path (default: cwd).
    #[arg(short = 'w', long, env = "TREEMAN_WORKTREE", value_hint = ValueHint::DirPath)]
    worktree: Option<PathBuf>,
}

// ───────────────────────── main ─────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    init_color(cli.no_color);
    let result = dispatch(cli).await;
    // anyhow chain with red error prefix when applicable.
    if let Err(e) = result {
        let prefix = paint("error:", Style::new().red().bold());
        eprintln!("{prefix} {e:#}");
        std::process::exit(1);
    }
    Ok(())
}

async fn dispatch(cli: Cli) -> Result<()> {
    match cli.cmd {
        Cmd::Status => status().await,
        Cmd::Slug(args) => slug(args),
        Cmd::Config(ConfigCmd::Validate { repo }) => config_validate(repo.repo),
        Cmd::Config(ConfigCmd::Show { repo, resolved }) => config_show(repo.repo, resolved),
        Cmd::Schema(SchemaCmd::Dump) => schema_dump(),
        Cmd::Schema(SchemaCmd::Install { dir }) => schema_install(dir),
        Cmd::Logs(LogsCmd::Tail {
            follow,
            limit,
            level,
            event_type,
            worktree,
        }) => {
            logs_tail(
                follow,
                limit,
                level.map(|l| l.as_str().into()),
                event_type,
                worktree,
            )
            .await
        }
        Cmd::Logs(LogsCmd::Grep {
            pattern,
            limit,
            level,
            event_type,
        }) => logs_grep(pattern, limit, level.map(|l| l.as_str().into()), event_type).await,
        Cmd::Worktree(WorktreeCmd::Create {
            branch,
            from,
            path,
            repo,
            skip_hooks,
            skip_prepare,
            foreground,
        }) => {
            worktree_create(
                branch,
                from,
                path,
                repo.repo,
                skip_hooks,
                skip_prepare,
                foreground,
            )
            .await
        }
        Cmd::Worktree(WorktreeCmd::Delete {
            target,
            repo,
            force,
            foreground,
        }) => worktree_delete(target, repo.repo, force, foreground).await,
        Cmd::Worktree(WorktreeCmd::Register { path, branch, repo }) => {
            worktree_register(path, branch, repo.repo).await
        }
        Cmd::Worktree(WorktreeCmd::List) => worktree_list().await,
        Cmd::Worktree(WorktreeCmd::Unregister { path }) => worktree_unregister(path).await,
        Cmd::Worktree(WorktreeCmd::Finalize { path, repo }) => {
            worktree_finalize(path, repo.repo).await
        }
        Cmd::Hook(HookCmd::Run { phase, worktree }) => {
            hook_run(phase.as_str().to_string(), worktree.worktree).await
        }
        Cmd::Db(DbCmd::Drop {
            engine,
            prefix,
            repo,
        }) => db_drop(engine.as_str().into(), prefix, repo.repo).await,
        Cmd::Db(DbCmd::Flush {
            engine,
            db,
            prefix,
            repo,
        }) => db_flush(engine.as_str().into(), db, prefix, repo.repo).await,
        Cmd::Db(DbCmd::List {
            engine,
            prefix,
            repo,
        }) => db_list(engine.as_str().into(), prefix, repo.repo).await,
        Cmd::Frameworks(FrameworksCmd::Detect { repo }) => frameworks_detect(repo.repo).await,
        Cmd::Watch(args) => watch(args).await,
        Cmd::Snapshot(SnapshotCmd::List { engine }) => {
            snapshot_list(engine.map(|e| e.as_str().into())).await
        }
        Cmd::Snapshot(SnapshotCmd::Show { fingerprint }) => snapshot_show(fingerprint).await,
        Cmd::Snapshot(SnapshotCmd::Gc {
            keep_per_source,
            max_age_days,
            max_total_gb,
        }) => snapshot_gc(keep_per_source, max_age_days, max_total_gb).await,
        Cmd::Prepare(args) => prepare_cmd(args).await,
        Cmd::Daemon(DaemonCmd::Start) => daemon_start().await,
        Cmd::Daemon(DaemonCmd::Stop) => daemon_stop().await,
        Cmd::Daemon(DaemonCmd::Restart) => daemon_restart().await,
        Cmd::Daemon(DaemonCmd::Status) => status().await,
        Cmd::Daemon(DaemonCmd::Install) => daemon_install(),
        Cmd::Daemon(DaemonCmd::Uninstall) => daemon_uninstall(),
        Cmd::Watcher(WatcherCmd::Start { common }) => watcher_start(common.repo).await,
        Cmd::Watcher(WatcherCmd::Stop { common }) => watcher_stop(common.repo).await,
        Cmd::Watcher(WatcherCmd::List) => watcher_list().await,
        Cmd::Watcher(WatcherCmd::Worktrees { common }) => watcher_worktrees(common.repo).await,
        Cmd::Init(args) => init_cmd(args),
        Cmd::Completions(args) => completions(args),
        Cmd::Manpage => manpage(),
    }
}

// ───────────────────────── color machinery ─────────────────────────

static mut COLOR_ON: bool = true;
fn init_color(no_color_flag: bool) {
    let env_no = std::env::var_os("NO_COLOR").is_some();
    let is_tty = std::io::stdout().is_terminal();
    unsafe {
        COLOR_ON = !no_color_flag && !env_no && is_tty;
    }
}
fn color_on() -> bool {
    unsafe { COLOR_ON }
}
fn paint(s: &str, st: Style) -> String {
    if color_on() {
        s.style(st).to_string()
    } else {
        s.to_string()
    }
}

// Style presets for CLI output. Keep semantic, not raw colors, so callers
// stay readable and we can tweak the palette in one place.
#[allow(dead_code)]
fn style_label() -> Style {
    Style::new().bold()
}
#[allow(dead_code)]
fn style_value() -> Style {
    Style::new().cyan()
}
#[allow(dead_code)]
fn style_path() -> Style {
    Style::new().cyan()
}
#[allow(dead_code)]
fn style_ok() -> Style {
    Style::new().green().bold()
}
#[allow(dead_code)]
fn style_warn() -> Style {
    Style::new().yellow()
}
#[allow(dead_code)]
fn style_dim() -> Style {
    Style::new().dimmed()
}
#[allow(dead_code)]
fn style_count() -> Style {
    Style::new().yellow().bold()
}
#[allow(dead_code)]
fn style_header() -> Style {
    Style::new().magenta().bold()
}

// ───────────────────────── completions / manpage ─────────────────────────

fn completions(args: CompletionsArgs) -> Result<()> {
    let mut cmd = Cli::command();
    let name = cmd.get_name().to_string();
    clap_complete::generate(args.shell, &mut cmd, name, &mut std::io::stdout());
    Ok(())
}

fn manpage() -> Result<()> {
    let cmd = Cli::command();
    let man = clap_mangen::Man::new(cmd);
    let mut buf = Vec::new();
    man.render(&mut buf)?;
    use std::io::Write;
    std::io::stdout().write_all(&buf)?;
    Ok(())
}

// ───────────────────────── daemon RPC commands ─────────────────────────

async fn status() -> Result<()> {
    match client::call(Request::Status).await? {
        Response::Status(s) => {
            let row = |label: &str, val: String| {
                println!(
                    "{:<15} {}",
                    paint(label, style_label()),
                    paint(&val, style_value())
                );
            };
            row("daemon_version:", s.daemon_version);
            row("protocol:", format!("v{}", s.protocol_version));
            row("pid:", s.pid.to_string());
            row("started_at:", s.started_at_unix.to_string());
            row("watchers:", s.watcher_count.to_string());
        }
        Response::Error { message } => bail!("daemon error: {message}"),
        other => bail!("unexpected response: {:?}", other),
    }
    Ok(())
}

async fn daemon_start() -> Result<()> {
    if let Ok(Response::Pong) = client::call(Request::Ping).await {
        println!("{} treemand already running", paint("ok:", style_ok()));
        return Ok(());
    }
    if service_installed() {
        return service_start();
    }
    spawn_treemand_detached().await
}

async fn daemon_stop() -> Result<()> {
    if service_installed() {
        return service_stop();
    }
    match client::call(Request::Shutdown).await {
        Ok(Response::Ok) => {
            println!("{} shutdown sent", paint("ok:", style_ok()));
            Ok(())
        }
        Ok(other) => bail!("unexpected response: {:?}", other),
        Err(e) => bail!("could not reach daemon: {e}"),
    }
}

async fn daemon_restart() -> Result<()> {
    // Best-effort stop; ignore failure when the daemon isn't running.
    let _ = daemon_stop().await;
    // Wait briefly for the socket to free up before starting again.
    for _ in 0..20 {
        if client::call(Request::Ping).await.is_err() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    }
    daemon_start().await
}

fn daemon_install() -> Result<()> {
    let exe = std::env::current_exe()?;
    let daemon_bin = exe.parent().context("exe parent")?.join("treemand");
    if !daemon_bin.is_file() {
        bail!(
            "treemand binary not found next to treeman at {}",
            daemon_bin.display()
        );
    }
    if cfg!(target_os = "macos") {
        install_launchd(&daemon_bin)
    } else if cfg!(target_os = "linux") {
        install_systemd(&daemon_bin)
    } else {
        bail!("daemon install is only implemented on Linux (systemd) and macOS (launchd)");
    }
}

fn daemon_uninstall() -> Result<()> {
    if cfg!(target_os = "macos") {
        uninstall_launchd()
    } else if cfg!(target_os = "linux") {
        uninstall_systemd()
    } else {
        bail!("daemon uninstall is only implemented on Linux (systemd) and macOS (launchd)");
    }
}

// ───────────────────────── service helpers ─────────────────────────

fn home() -> Result<PathBuf> {
    Ok(PathBuf::from(std::env::var("HOME")?))
}

fn systemd_unit_path() -> Result<PathBuf> {
    Ok(home()?.join(".config/systemd/user/treemand.service"))
}

fn launchd_plist_path() -> Result<PathBuf> {
    Ok(home()?.join("Library/LaunchAgents/com.treeman.daemon.plist"))
}

fn launchd_label() -> &'static str {
    "com.treeman.daemon"
}

fn service_installed() -> bool {
    if cfg!(target_os = "macos") {
        launchd_plist_path().map(|p| p.is_file()).unwrap_or(false)
    } else if cfg!(target_os = "linux") {
        systemd_unit_path().map(|p| p.is_file()).unwrap_or(false)
    } else {
        false
    }
}

fn service_start() -> Result<()> {
    if cfg!(target_os = "macos") {
        let plist = launchd_plist_path()?;
        let uid = unsafe { libc::geteuid() }.to_string();
        // `launchctl bootstrap` loads + starts the service; idempotent
        // re-bootstrap fails, so try kickstart -k as fallback.
        let s = std::process::Command::new("launchctl")
            .args(["bootstrap", &format!("gui/{uid}")])
            .arg(&plist)
            .status();
        if !s.map(|s| s.success()).unwrap_or(false) {
            let _ = std::process::Command::new("launchctl")
                .args(["kickstart", "-k", &format!("gui/{uid}/{}", launchd_label())])
                .status();
        }
        println!(
            "{} treemand started via launchd ({})",
            paint("ok:", style_ok()),
            paint(launchd_label(), style_dim())
        );
        Ok(())
    } else {
        let s = std::process::Command::new("systemctl")
            .args(["--user", "start", "treemand.service"])
            .status()
            .context("systemctl --user start")?;
        if !s.success() {
            bail!("systemctl --user start treemand.service failed");
        }
        println!(
            "{} treemand started via {}",
            paint("ok:", style_ok()),
            paint("systemd --user", style_dim())
        );
        Ok(())
    }
}

fn service_stop() -> Result<()> {
    if cfg!(target_os = "macos") {
        let plist = launchd_plist_path()?;
        let uid = unsafe { libc::geteuid() }.to_string();
        let _ = std::process::Command::new("launchctl")
            .args(["bootout", &format!("gui/{uid}")])
            .arg(&plist)
            .status();
        println!(
            "{} treemand stopped via {}",
            paint("ok:", style_ok()),
            paint("launchd", style_dim())
        );
        Ok(())
    } else {
        let s = std::process::Command::new("systemctl")
            .args(["--user", "stop", "treemand.service"])
            .status()
            .context("systemctl --user stop")?;
        if !s.success() {
            bail!("systemctl --user stop treemand.service failed");
        }
        println!(
            "{} treemand stopped via {}",
            paint("ok:", style_ok()),
            paint("systemd --user", style_dim())
        );
        Ok(())
    }
}

async fn spawn_treemand_detached() -> Result<()> {
    let exe = std::env::current_exe()?;
    let daemon_bin = exe.parent().context("exe parent")?.join("treemand");
    if !daemon_bin.is_file() {
        bail!(
            "treemand binary not found next to treeman at {}",
            daemon_bin.display()
        );
    }
    // setsid on Linux; nohup on macOS (setsid is not on the default PATH).
    if cfg!(target_os = "macos") {
        std::process::Command::new("nohup")
            .arg(&daemon_bin)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .spawn()
            .context("spawn treemand via nohup")?;
    } else {
        std::process::Command::new("setsid")
            .arg(&daemon_bin)
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .spawn()
            .context("spawn treemand via setsid")?;
    }
    for _ in 0..50 {
        if let Ok(Response::Pong) = client::call(Request::Ping).await {
            println!(
                "{} treemand started",
                paint("ok", Style::new().green().bold())
            );
            return Ok(());
        }
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    }
    bail!("treemand failed to come up within 5s");
}

fn install_systemd(daemon_bin: &Path) -> Result<()> {
    let unit_path = systemd_unit_path()?;
    if let Some(parent) = unit_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let unit = format!(
        r#"[Unit]
Description=Treeman per-worktree DB orchestrator daemon
After=default.target

[Service]
Type=simple
ExecStart={daemon}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
"#,
        daemon = daemon_bin.display()
    );
    std::fs::write(&unit_path, unit).with_context(|| format!("write {}", unit_path.display()))?;
    println!(
        "{} wrote {}",
        paint("ok:", style_ok()),
        paint(&unit_path.display().to_string(), style_path())
    );
    run_systemctl(&["--user", "daemon-reload"])?;
    run_systemctl(&["--user", "enable", "--now", "treemand.service"])?;
    println!(
        "treemand enabled + started (systemd --user). Check: `systemctl --user status treemand`"
    );
    Ok(())
}

fn uninstall_systemd() -> Result<()> {
    let unit_path = systemd_unit_path()?;
    let existed = unit_path.is_file();
    // Stop + disable; ignore failures (unit may already be gone).
    let _ = std::process::Command::new("systemctl")
        .args(["--user", "disable", "--now", "treemand.service"])
        .status();
    if existed {
        std::fs::remove_file(&unit_path)
            .with_context(|| format!("remove {}", unit_path.display()))?;
        println!(
            "{} removed {}",
            paint("ok:", style_ok()),
            paint(&unit_path.display().to_string(), style_path())
        );
    }
    let _ = std::process::Command::new("systemctl")
        .args(["--user", "daemon-reload"])
        .status();
    if !existed {
        println!(
            "{}",
            paint(
                &format!("no systemd user unit at {}", unit_path.display()),
                style_dim()
            )
        );
    }
    Ok(())
}

fn run_systemctl(args: &[&str]) -> Result<()> {
    let s = std::process::Command::new("systemctl")
        .args(args)
        .status()
        .context("invoke systemctl")?;
    if !s.success() {
        bail!("systemctl {args:?} failed");
    }
    Ok(())
}

fn install_launchd(daemon_bin: &Path) -> Result<()> {
    let plist_path = launchd_plist_path()?;
    if let Some(parent) = plist_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let label = launchd_label();
    let log_dir = home()?.join("Library/Logs/treeman");
    std::fs::create_dir_all(&log_dir)?;
    let plist = format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{label}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{daemon}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{log}/treemand.out.log</string>
    <key>StandardErrorPath</key>
    <string>{log}/treemand.err.log</string>
    <key>ProcessType</key>
    <string>Interactive</string>
</dict>
</plist>
"#,
        daemon = daemon_bin.display(),
        log = log_dir.display()
    );
    std::fs::write(&plist_path, plist)
        .with_context(|| format!("write {}", plist_path.display()))?;
    println!(
        "{} wrote {}",
        paint("ok:", style_ok()),
        paint(&plist_path.display().to_string(), style_path())
    );
    let uid = unsafe { libc::geteuid() }.to_string();
    let domain = format!("gui/{uid}");
    // bootstrap = load + run; if already loaded, kickstart -k restarts it.
    let s = std::process::Command::new("launchctl")
        .args(["bootstrap", &domain])
        .arg(&plist_path)
        .status()
        .context("invoke launchctl bootstrap")?;
    if !s.success() {
        let _ = std::process::Command::new("launchctl")
            .args(["kickstart", "-k", &format!("{domain}/{label}")])
            .status();
    }
    println!(
        "treemand loaded into launchd ({label}). Logs: {}",
        log_dir.display()
    );
    Ok(())
}

fn uninstall_launchd() -> Result<()> {
    let plist_path = launchd_plist_path()?;
    let existed = plist_path.is_file();
    let uid = unsafe { libc::geteuid() }.to_string();
    let domain = format!("gui/{uid}");
    if existed {
        let _ = std::process::Command::new("launchctl")
            .args(["bootout", &domain])
            .arg(&plist_path)
            .status();
        std::fs::remove_file(&plist_path)
            .with_context(|| format!("remove {}", plist_path.display()))?;
        println!(
            "{} removed {}",
            paint("ok:", style_ok()),
            paint(&plist_path.display().to_string(), style_path())
        );
    } else {
        println!(
            "{}",
            paint(
                &format!("no LaunchAgent at {}", plist_path.display()),
                style_dim()
            )
        );
    }
    Ok(())
}

async fn watcher_start(repo: Option<PathBuf>) -> Result<()> {
    let repo_path = resolve_repo_for_watcher(repo)?;
    let req = Request::WatcherStart {
        repo_path: repo_path.to_string_lossy().to_string(),
    };
    match client::call(req).await? {
        Response::WatcherStarted { repo_path } => println!(
            "{} watcher started: {}",
            paint("ok:", style_ok()),
            paint(&repo_path, style_path())
        ),
        Response::Error { message } => bail!("daemon: {message}"),
        other => bail!("unexpected: {other:?}"),
    }
    Ok(())
}

async fn watcher_stop(repo: Option<PathBuf>) -> Result<()> {
    let repo_path = resolve_repo_for_watcher(repo)?;
    let req = Request::WatcherStop {
        repo_path: repo_path.to_string_lossy().to_string(),
    };
    match client::call(req).await? {
        Response::WatcherStopped { repo_path } => println!(
            "{} watcher stopped: {}",
            paint("ok:", style_ok()),
            paint(&repo_path, style_path())
        ),
        Response::Error { message } => bail!("daemon: {message}"),
        other => bail!("unexpected: {other:?}"),
    }
    Ok(())
}

async fn watcher_list() -> Result<()> {
    match client::call(Request::WatcherList).await? {
        Response::WatcherList { repos } => {
            if repos.is_empty() {
                println!("{}", paint("(no watchers running)", style_dim()));
            } else {
                for r in repos {
                    println!(
                        "{}  {} {}",
                        paint(&r.repo, style_path()),
                        paint(&r.worktree_count.to_string(), style_count()),
                        paint("worktrees", style_dim()),
                    );
                }
            }
        }
        Response::Error { message } => bail!("daemon: {message}"),
        other => bail!("unexpected: {other:?}"),
    }
    Ok(())
}

async fn watcher_worktrees(repo: Option<PathBuf>) -> Result<()> {
    let repo_root = resolve_repo_for_watcher(repo)?;
    let req = Request::WorktreeList {
        repo_path: repo_root.to_string_lossy().to_string(),
    };
    match client::call(req).await? {
        Response::WorktreeList { worktrees } => {
            if worktrees.is_empty() {
                println!(
                    "{}",
                    paint("(no per-worktree watchers running)", style_dim())
                );
            } else {
                for w in worktrees {
                    println!("{}", paint(&w, style_path()));
                }
            }
        }
        Response::Error { message } => bail!("daemon: {message}"),
        other => bail!("unexpected: {other:?}"),
    }
    Ok(())
}

fn resolve_repo_for_watcher(repo: Option<PathBuf>) -> Result<PathBuf> {
    match repo {
        Some(p) => p
            .canonicalize()
            .with_context(|| format!("canonicalize {}", p.display())),
        None => {
            let cwd = std::env::current_dir()?;
            discover_repo_root(&cwd).context("no repo root found")
        }
    }
}

// ───────────────────────── slug / config / schema ─────────────────────────

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
    let cfg = treeman_core::config::load_layered(repo.as_deref()).context("load config")?;
    let ok = paint("ok:", Style::new().green().bold());
    println!(
        "{ok} config loaded ({} databases configured)",
        cfg.databases.len()
    );
    Ok(())
}

fn config_show(repo: Option<PathBuf>, resolved: bool) -> Result<()> {
    let repo = resolve_repo(repo)?;
    let cfg = treeman_core::config::load_layered(repo.as_deref())?;
    print!("{}", serde_yaml::to_string(&cfg)?);
    if resolved {
        use treeman_core::resolve::{Source, resolve};
        // Resolve from the raw (pre-credential-merge) config so the
        // `Source` labels accurately point at the file that supplied
        // each connection, rather than always reporting `yaml`.
        let raw = treeman_core::config::load_layered_raw(repo.as_deref())?;
        let r = resolve(&raw, repo.as_deref());
        println!();
        let hdr = paint(
            "# resolved connections (yaml < env-url < repo-env-file)",
            Style::new().bold(),
        );
        println!("{hdr}");
        emit_resolved("mysql", r.mysql.map(|(c, s)| (format!("{c:?}"), s)));
        emit_resolved("postgres", r.postgres.map(|(c, s)| (format!("{c:?}"), s)));
        emit_resolved("mongodb", r.mongodb.map(|(c, s)| (format!("{c:?}"), s)));
        emit_resolved("redis", r.redis.map(|(c, s)| (format!("{c:?}"), s)));
        emit_resolved(
            "elasticsearch",
            r.elasticsearch.map(|(c, s)| (format!("{c:?}"), s)),
        );

        fn emit_resolved(name: &str, entry: Option<(String, Source)>) {
            match entry {
                Some((debug_str, src)) => {
                    let label = match src {
                        Source::Yaml => "yaml".to_string(),
                        Source::EnvUrl(k) => format!("env:{k}"),
                        Source::DatabaseUrl => "env:DATABASE_URL".into(),
                        Source::RepoEnvFile(p) => format!("file:{}", p.display()),
                        Source::Default => "default".into(),
                    };
                    println!("# {name} <- {label}");
                    println!("# {debug_str}");
                }
                None => println!("# {name} <- (none)"),
            }
        }
    }
    Ok(())
}

fn schema_dump() -> Result<()> {
    let s = treeman_core::config::json_schema();
    println!("{}", serde_json::to_string_pretty(&s)?);
    Ok(())
}

fn schema_install(dir: Option<PathBuf>) -> Result<()> {
    let dir = match dir {
        Some(d) => d,
        None => {
            let home = std::env::var("HOME").context("HOME unset")?;
            let xdg =
                std::env::var("XDG_CONFIG_HOME").unwrap_or_else(|_| format!("{home}/.config"));
            PathBuf::from(xdg).join("treeman/schemas")
        }
    };
    std::fs::create_dir_all(&dir).with_context(|| format!("create {}", dir.display()))?;
    let schema = treeman_core::config::json_schema();
    let global = dir.join("global.schema.json");
    let repo = dir.join("repo.schema.json");
    let pretty = serde_json::to_string_pretty(&schema)?;
    std::fs::write(&global, &pretty).with_context(|| format!("write {}", global.display()))?;
    std::fs::write(&repo, &pretty).with_context(|| format!("write {}", repo.display()))?;

    println!(
        "{} wrote {}",
        paint("ok:", style_ok()),
        paint(&global.display().to_string(), style_path())
    );
    println!(
        "{} wrote {}",
        paint("ok:", style_ok()),
        paint(&repo.display().to_string(), style_path())
    );
    println!();
    println!("To enable LSP completions in your editor, add a modeline to the top of");
    println!("each YAML config file:");
    println!();
    let repo_url = format!("file://{}", repo.display());
    let global_url = format!("file://{}", global.display());
    let h1 = paint("# In .treeman.yaml (per-repo):", Style::new().bold());
    let h2 = paint(
        "# In ~/.config/treeman/config.yaml (global):",
        Style::new().bold(),
    );
    println!("{h1}");
    println!("# yaml-language-server: $schema={repo_url}");
    println!();
    println!("{h2}");
    println!("# yaml-language-server: $schema={global_url}");
    println!();
    println!("VS Code (with redhat.vscode-yaml) can also map schemas via settings.json:");
    println!();
    println!(r#"  "yaml.schemas": {{"#);
    println!(r#"    "{global_url}": "~/.config/treeman/config.yaml","#);
    println!(r#"    "{repo_url}": "**/.treeman.yaml""#);
    println!(r#"  }}"#);
    Ok(())
}

fn resolve_repo(explicit: Option<PathBuf>) -> Result<Option<PathBuf>> {
    if let Some(p) = explicit {
        return Ok(Some(p));
    }
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

// ───────────────────────── logs ─────────────────────────

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
        limit: Some(limit),
        level: level.clone(),
        event_type: event_type.clone(),
        worktree_id: worktree,
        ..Default::default()
    };
    let mut rows = query_events(&pool, &filter).await?;
    rows.reverse();
    let mut last_id = rows.iter().map(|r| r.id).max().unwrap_or(0);
    for r in &rows {
        print_event(r);
    }
    if !follow {
        return Ok(());
    }
    loop {
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;
        let next = tail_events(&pool, last_id).await?;
        for r in &next {
            if let Some(ref l) = level {
                if &r.level != l {
                    continue;
                }
            }
            if let Some(ref t) = event_type {
                if &r.event_type != t {
                    continue;
                }
            }
            if let Some(w) = worktree {
                if r.worktree_id != Some(w) {
                    continue;
                }
            }
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
        limit: Some(limit),
        level,
        event_type,
        grep: Some(pattern),
        ..Default::default()
    };
    let mut rows = query_events(&pool, &filter).await?;
    rows.reverse();
    for r in &rows {
        print_event(r);
    }
    Ok(())
}

fn print_event(r: &treeman_store::EventRow) {
    let ts = chrono::DateTime::from_timestamp_millis(r.ts)
        .map(|t| t.format("%Y-%m-%d %H:%M:%S%.3f").to_string())
        .unwrap_or_else(|| r.ts.to_string());
    let lvl_style = match r.level.as_str() {
        "debug" => Style::new().dimmed(),
        "info" => Style::new().green(),
        "warn" => Style::new().yellow().bold(),
        "error" => Style::new().red().bold(),
        _ => Style::new(),
    };
    let lvl = paint(&r.level.to_uppercase(), lvl_style);
    let etype = paint(&r.event_type, Style::new().cyan());
    let msg = r.message.as_deref().unwrap_or("");
    let ts_p = paint(&ts, Style::new().dimmed());
    println!("{ts_p} {lvl:5} {etype} {msg}");
}

// ───────────────────────── hook + worktree register ─────────────────────────

async fn worktree_register(
    path: PathBuf,
    branch: Option<String>,
    repo: Option<PathBuf>,
) -> Result<()> {
    let path = path
        .canonicalize()
        .with_context(|| format!("canonicalize {}", path.display()))?;
    let repo_root = match repo {
        Some(r) => r.canonicalize()?,
        None => discover_repo_root(&path).context("could not find repo root for worktree")?,
    };
    let repo_name = repo_root
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("repo")
        .to_string();
    let branch = branch.or_else(|| detect_branch(&path));
    let slug = treeman_core::slug_for(&path, branch.as_deref());

    let pool = open_pool().await?;
    let repo_id = treeman_store::ensure_repo(&pool, &repo_root, &repo_name).await?;
    let wt_id =
        treeman_store::ensure_worktree(&pool, repo_id, &path, &slug.value, branch.as_deref())
            .await?;
    println!(
        "worktree #{} slug={} repo=#{} ({})",
        wt_id,
        slug.value,
        repo_id,
        repo_root.display()
    );
    Ok(())
}

async fn worktree_list() -> Result<()> {
    let pool = open_pool().await?;
    let rows = treeman_store::hook_runs::list_worktrees(&pool).await?;
    if rows.is_empty() {
        println!("{}", paint("(no worktrees registered)", style_dim()));
        return Ok(());
    }
    let hdr = paint(
        &format!("{:<4} {:<24} {:<24} {}", "ID", "SLUG", "BRANCH", "PATH"),
        Style::new().bold(),
    );
    println!("{hdr}");
    for r in rows {
        println!(
            "{:<4} {:<24} {:<24} {}",
            r.id,
            r.slug,
            r.branch.as_deref().unwrap_or("-"),
            r.path
        );
    }
    Ok(())
}

async fn worktree_unregister(path: PathBuf) -> Result<()> {
    let path = path
        .canonicalize()
        .with_context(|| format!("canonicalize {}", path.display()))?;
    let pool = open_pool().await?;
    let wt = treeman_store::hook_runs::find_worktree_by_path(&pool, &path.to_string_lossy())
        .await?
        .with_context(|| format!("worktree not registered: {}", path.display()))?;
    treeman_store::mark_worktree_deleted(&pool, wt.id).await?;
    println!(
        "{} unregistered worktree {} ({})",
        paint("ok:", style_ok()),
        paint(&format!("#{}", wt.id), style_count()),
        paint(&wt.path, style_path())
    );
    Ok(())
}

async fn hook_run(phase: String, worktree: Option<PathBuf>) -> Result<()> {
    let wt_path = match worktree {
        Some(p) => p.canonicalize()?,
        None => std::env::current_dir()?,
    };
    let repo_root =
        discover_repo_root(&wt_path).context("could not find repo root containing worktree")?;
    let cfg = treeman_core::config::load_layered(Some(&repo_root))?;
    let branch = detect_branch(&wt_path);
    let slug = treeman_core::slug_for(&wt_path, branch.as_deref());
    let env = capture_inherited_env();

    let pool = open_pool().await?;
    let repo_name = repo_root
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("repo");
    let repo_id = treeman_store::ensure_repo(&pool, &repo_root, repo_name).await?;
    let wt_id =
        treeman_store::ensure_worktree(&pool, repo_id, &wt_path, &slug.value, branch.as_deref())
            .await?;
    let run_id = treeman_store::hook_runs::start_hook_run(&pool, wt_id, &phase).await?;

    let start = std::time::Instant::now();
    let outcome = match phase.as_str() {
        "precreate" => {
            treeman_core::hooks::run_precreate_hooks(
                &cfg.hooks.precreate,
                &repo_root,
                &wt_path,
                &slug.value,
                &env,
            )
            .await?
        }
        "postcreate" => {
            treeman_core::hooks::run_hooks(
                "postcreate",
                &cfg.hooks.postcreate,
                &repo_root,
                &wt_path,
                &slug.value,
                &env,
            )
            .await?
        }
        "predelete" => {
            treeman_core::hooks::run_hooks(
                "predelete",
                &cfg.hooks.predelete,
                &repo_root,
                &wt_path,
                &slug.value,
                &env,
            )
            .await?
        }
        "postdelete" => {
            treeman_core::hooks::run_hooks(
                "postdelete",
                &cfg.hooks.postdelete,
                &repo_root,
                &wt_path,
                &slug.value,
                &env,
            )
            .await?
        }
        other => bail!("unknown hook phase: {other}"),
    };
    let duration_ms = start.elapsed().as_millis() as i64;
    let mut stdout = String::new();
    let mut stderr = String::new();
    for (i, s) in outcome.groups.iter().enumerate() {
        stdout.push_str(&format!(
            "--- group {i} (pid={:?}, log={}) ---\n{}\n",
            s.pid,
            s.log_path.display(),
            s.stdout_tail
        ));
        if !s.stderr_tail.is_empty() {
            stderr.push_str(&format!("--- group {i} stderr ---\n{}\n", s.stderr_tail));
        }
        let payload = serde_json::json!({
            "command": s.command,
            "exit_code": s.exit_code,
            "pid": s.pid,
            "log_path": s.log_path.to_string_lossy(),
            "stdout_tail": s.stdout_tail,
            "stderr_tail": s.stderr_tail,
        })
        .to_string();
        let level = if s.exit_code == 0 { "info" } else { "error" };
        treeman_store::write_event(
            &pool,
            level,
            "hook_step",
            Some(&s.command),
            Some(repo_id),
            Some(wt_id),
            Some(&phase),
            None,
            &payload,
        )
        .await?;
    }
    treeman_store::hook_runs::finish_hook_run(
        &pool,
        run_id,
        outcome.aggregate_exit_code,
        &stdout,
        &stderr,
    )
    .await?;
    let summary = serde_json::json!({
        "run_id": run_id, "exit_code": outcome.aggregate_exit_code, "group_count": outcome.groups.len()
    }).to_string();
    let level = if outcome.aggregate_exit_code == 0 {
        "info"
    } else {
        "error"
    };
    treeman_store::write_event(
        &pool,
        level,
        "hook_run",
        Some(&format!(
            "hook {} → exit {}",
            phase, outcome.aggregate_exit_code
        )),
        Some(repo_id),
        Some(wt_id),
        Some(&phase),
        Some(duration_ms),
        &summary,
    )
    .await?;

    if phase == "predelete" {
        teardown_databases(&cfg, &slug.value, repo_id, wt_id, &pool).await?;
    }

    let label = if outcome.aggregate_exit_code == 0 {
        paint("ok", Style::new().green().bold())
    } else {
        paint("err", Style::new().red().bold())
    };
    println!(
        "{label} hook_run #{run_id} phase={phase} exit={}",
        outcome.aggregate_exit_code
    );
    if outcome.aggregate_exit_code != 0 {
        std::process::exit(outcome.aggregate_exit_code);
    }
    Ok(())
}

// ───────────────────────── db ─────────────────────────

async fn db_drop(engine: String, prefix: String, repo: Option<PathBuf>) -> Result<()> {
    let cfg = load_cfg(repo)?;
    let driver = open_driver(&engine, &cfg)?;
    let dropped = driver.drop_matching(&prefix).await?;
    if dropped.is_empty() {
        println!("(no databases matched {prefix}*)");
    } else {
        for n in dropped {
            println!("dropped {n}");
        }
    }
    Ok(())
}

async fn db_flush(
    engine: String,
    db: Option<u8>,
    prefix: Option<String>,
    repo: Option<PathBuf>,
) -> Result<()> {
    let cfg = load_cfg(repo)?;
    let driver = open_driver(&engine, &cfg)?;
    if let Some(idx) = db {
        driver
            .flush_namespace(&treeman_db::Namespace::RedisDb(idx))
            .await?;
        println!("flushed redis db {idx}");
    } else if let Some(p) = prefix {
        for name in driver.list_matching(&p).await? {
            driver
                .flush_namespace(&treeman_db::Namespace::Database(name.clone()))
                .await?;
            println!("flushed {name}");
        }
    } else {
        bail!("--db or --prefix required");
    }
    Ok(())
}

async fn db_list(engine: String, prefix: String, repo: Option<PathBuf>) -> Result<()> {
    let cfg = load_cfg(repo)?;
    let driver = open_driver(&engine, &cfg)?;
    for n in driver.list_matching(&prefix).await? {
        println!("{n}");
    }
    Ok(())
}

fn load_cfg(repo: Option<PathBuf>) -> Result<treeman_core::Config> {
    let repo_root = match repo {
        Some(p) => Some(p.canonicalize()?),
        None => discover_repo_root(&std::env::current_dir()?),
    };
    treeman_core::config::load_layered(repo_root.as_deref())
}

fn open_driver(engine: &str, cfg: &treeman_core::Config) -> Result<Box<dyn treeman_db::DbDriver>> {
    use treeman_db::*;
    match engine {
        "mysql" => {
            let mc = cfg
                .connections
                .mysql
                .clone()
                .context("connections.mysql not configured")?;
            let rt = tokio::runtime::Handle::current();
            Ok(Box::new(rt.block_on(mysql::MysqlDriver::connect(&mc))?))
        }
        "postgres" => {
            let pc = cfg
                .connections
                .postgres
                .clone()
                .context("connections.postgres not configured")?;
            let rt = tokio::runtime::Handle::current();
            Ok(Box::new(
                rt.block_on(postgres::PostgresDriver::connect(&pc))?,
            ))
        }
        "mongodb" => {
            let mc = cfg
                .connections
                .mongodb
                .clone()
                .context("connections.mongodb not configured")?;
            let rt = tokio::runtime::Handle::current();
            Ok(Box::new(rt.block_on(mongo::MongoDriver::connect(&mc))?))
        }
        "elasticsearch" => {
            let ec = cfg
                .connections
                .elasticsearch
                .clone()
                .context("connections.elasticsearch not configured")?;
            Ok(Box::new(elasticsearch::ElasticsearchDriver::connect(&ec)?))
        }
        "redis" => {
            let rc = cfg
                .connections
                .redis
                .clone()
                .context("connections.redis not configured")?;
            Ok(Box::new(redis_driver::RedisDriver::connect(&rc)?))
        }
        // Wire-compat variants
        "mariadb" | "tidb" => {
            let mc = cfg
                .connections
                .mysql
                .clone()
                .context("connections.mysql not configured")?;
            let rt = tokio::runtime::Handle::current();
            Ok(Box::new(rt.block_on(mysql::MysqlDriver::connect(&mc))?))
        }
        "cockroach" => {
            let pc = cfg
                .connections
                .postgres
                .clone()
                .context("connections.postgres not configured")?;
            let rt = tokio::runtime::Handle::current();
            Ok(Box::new(
                rt.block_on(postgres::PostgresDriver::connect(&pc))?,
            ))
        }
        "opensearch" => {
            let ec = cfg
                .connections
                .elasticsearch
                .clone()
                .context("connections.elasticsearch not configured")?;
            Ok(Box::new(elasticsearch::ElasticsearchDriver::connect(&ec)?))
        }
        "clickhouse" => {
            let hc = cfg
                .connections
                .clickhouse
                .clone()
                .context("connections.clickhouse not configured")?;
            let pw = hc
                .password_env
                .as_deref()
                .and_then(|e| std::env::var(e).ok());
            Ok(Box::new(clickhouse::ClickhouseDriver::connect(
                &hc.url,
                hc.user.as_deref(),
                pw.as_deref(),
            )?))
        }
        "duckdb" => {
            let dc = cfg
                .connections
                .duckdb
                .clone()
                .context("connections.duckdb not configured")?;
            Ok(Box::new(duckdb_driver::DuckdbDriver::new(&dc.base_dir)?))
        }
        "meilisearch" => {
            let hc = cfg
                .connections
                .meilisearch
                .clone()
                .context("connections.meilisearch not configured")?;
            let k = hc
                .api_key_env
                .as_deref()
                .and_then(|e| std::env::var(e).ok());
            Ok(Box::new(http_engines::MeilisearchDriver::connect(
                &hc.url,
                k.as_deref(),
            )?))
        }
        "typesense" => {
            let hc = cfg
                .connections
                .typesense
                .clone()
                .context("connections.typesense not configured")?;
            let k = hc
                .api_key_env
                .as_deref()
                .and_then(|e| std::env::var(e).ok())
                .context("typesense api_key_env not set in environment")?;
            Ok(Box::new(http_engines::TypesenseDriver::connect(
                &hc.url, &k,
            )?))
        }
        "qdrant" => {
            let hc = cfg
                .connections
                .qdrant
                .clone()
                .context("connections.qdrant not configured")?;
            let k = hc
                .api_key_env
                .as_deref()
                .and_then(|e| std::env::var(e).ok());
            Ok(Box::new(http_engines::QdrantDriver::connect(
                &hc.url,
                k.as_deref(),
            )?))
        }
        "weaviate" => {
            let hc = cfg
                .connections
                .weaviate
                .clone()
                .context("connections.weaviate not configured")?;
            let k = hc
                .api_key_env
                .as_deref()
                .and_then(|e| std::env::var(e).ok());
            Ok(Box::new(http_engines::WeaviateDriver::connect(
                &hc.url,
                k.as_deref(),
            )?))
        }
        "milvus" => {
            let hc = cfg
                .connections
                .milvus
                .clone()
                .context("connections.milvus not configured")?;
            let k = hc
                .api_key_env
                .as_deref()
                .and_then(|e| std::env::var(e).ok());
            Ok(Box::new(http_engines::MilvusDriver::connect(
                &hc.url,
                k.as_deref(),
            )?))
        }
        "neo4j" => {
            let nc = cfg
                .connections
                .neo4j
                .clone()
                .context("connections.neo4j not configured")?;
            let pw = std::env::var(&nc.password_env)
                .with_context(|| format!("neo4j password_env {} not set", nc.password_env))?;
            Ok(Box::new(neo4j::Neo4jDriver::connect(
                &nc.url, &nc.user, &pw,
            )?))
        }
        "influxdb" => {
            let ic = cfg
                .connections
                .influxdb
                .clone()
                .context("connections.influxdb not configured")?;
            let token = std::env::var(&ic.token_env)
                .with_context(|| format!("influxdb token_env {} not set", ic.token_env))?;
            Ok(Box::new(influxdb::InfluxdbDriver::connect(
                &ic.url, &token, &ic.org_id,
            )?))
        }
        "memcached" => {
            let mc = cfg
                .connections
                .memcached
                .clone()
                .context("connections.memcached not configured")?;
            Ok(Box::new(memcached::MemcachedDriver::connect(
                mc.host, mc.port,
            )))
        }
        "rabbitmq" => {
            let hc = cfg
                .connections
                .rabbitmq
                .clone()
                .context("connections.rabbitmq not configured")?;
            let user = hc.user.clone().context("rabbitmq user missing")?;
            let pw = hc
                .password_env
                .as_deref()
                .and_then(|e| std::env::var(e).ok())
                .context("rabbitmq password_env not set")?;
            Ok(Box::new(rabbitmq::RabbitmqDriver::connect(
                &hc.url, &user, &pw,
            )?))
        }
        "nats" => {
            let hc = cfg
                .connections
                .nats
                .clone()
                .context("connections.nats not configured")?;
            Ok(Box::new(http_engines::NatsDriver::connect(&hc.url)?))
        }
        "etcd" => {
            let hc = cfg
                .connections
                .etcd
                .clone()
                .context("connections.etcd not configured")?;
            Ok(Box::new(etcd::EtcdDriver::connect(&hc.url)?))
        }
        "kafka" => {
            let hp = cfg
                .connections
                .kafka
                .clone()
                .context("connections.kafka not configured")?;
            // Kafka driver expects a REST Proxy URL; build one if user
            // gave only host:port.
            let url = format!("http://{}:{}", hp.host, hp.port);
            Ok(Box::new(http_engines::KafkaDriver::connect(&url)?))
        }
        "s3" => {
            let sc = cfg
                .connections
                .s3
                .clone()
                .context("connections.s3 not configured")?;
            Ok(Box::new(s3::S3Driver::connect(&sc.endpoint, &sc.region)))
        }
        other => bail!("unsupported engine: {other}"),
    }
}

// ───────────────────────── frameworks / watch ─────────────────────────

async fn frameworks_detect(repo: Option<PathBuf>) -> Result<()> {
    let repo_root = match repo {
        Some(p) => p.canonicalize()?,
        None => discover_repo_root(&std::env::current_dir()?).context("no repo root found")?,
    };
    let cfg = treeman_core::config::load_layered(Some(&repo_root))?;
    let registry = treeman_migrations::Registry::with_builtins().merge_yaml(&cfg.frameworks);

    // Migration frameworks
    let mig = registry.detect_all(&repo_root);
    if mig.is_empty() {
        println!("migration frameworks: (none detected)");
    } else {
        let hdr = paint(
            &format!(
                "{:<18} {:<14} {:<10} {}",
                "MIGRATION_FW", "HASH_MODE", "ON_MODIFY", "DIRS"
            ),
            Style::new().bold(),
        );
        println!("{hdr}");
        for s in mig {
            let dirs: Vec<_> = s
                .migration_dirs(&repo_root)
                .iter()
                .map(|p| {
                    p.strip_prefix(&repo_root)
                        .unwrap_or(p)
                        .display()
                        .to_string()
                })
                .collect();
            println!(
                "{:<18} {:<14} {:<10} {}",
                s.name,
                format!("{:?}", s.hash_mode).to_lowercase(),
                format!("{:?}", s.on_modify).to_lowercase(),
                dirs.join(", ")
            );
        }
    }
    println!();

    // Test frameworks
    let tfw = treeman_migrations::testfw::detect_all(&repo_root);
    if tfw.is_empty() {
        println!("test frameworks: (none detected)");
    } else {
        let hdr = paint(
            &format!(
                "{:<22} {:<10} {:<14} {:<10} {}",
                "TEST_FW", "LANGUAGE", "STRATEGY", "WORKER_IDX", "WORKER_ENV"
            ),
            Style::new().bold(),
        );
        println!("{hdr}");
        for t in tfw {
            let strategy = format!("{:?}", t.clone_strategy).to_lowercase();
            let idx = format!("{:?}", t.worker_index).to_lowercase();
            let env = t.worker_env.unwrap_or_else(|| "-".into());
            println!(
                "{:<22} {:<10} {:<14} {:<10} {}",
                t.name, t.language, strategy, idx, env
            );
        }
        if let Some(n) = treeman_migrations::testfw::detected_clone_count(&repo_root) {
            println!();
            println!("auto-clones (replication target): {n}");
        }
    }
    Ok(())
}

async fn watch(args: WatchArgs) -> Result<()> {
    let repo_root = match args.repo.repo {
        Some(p) => p.canonicalize()?,
        None => discover_repo_root(&std::env::current_dir()?).context("no repo root found")?,
    };
    let cfg = treeman_core::config::load_layered(Some(&repo_root))?;
    let registry = treeman_migrations::Registry::with_builtins().merge_yaml(&cfg.frameworks);
    let detected: Vec<_> = registry
        .detect_all(&repo_root)
        .into_iter()
        .cloned()
        .collect();
    if detected.is_empty() {
        bail!("no migration frameworks detected — nothing to watch");
    }
    println!(
        "watching {} ({} framework(s))",
        repo_root.display(),
        detected.len()
    );

    let (tx, mut rx) = tokio::sync::mpsc::channel(64);
    let _handles = treeman_watcher::spawn_repo_watcher(
        repo_root.clone(),
        detected,
        cfg.watcher.debounce_ms,
        tx,
    )
    .await?;
    let pool = open_pool().await?;
    let repo_name = repo_root
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("repo");
    let repo_id = treeman_store::ensure_repo(&pool, &repo_root, repo_name).await?;
    let wt_path = std::env::current_dir()?;
    let branch = detect_branch(&wt_path);
    let slug = treeman_core::slug_for(&wt_path, branch.as_deref());
    let wt_id =
        treeman_store::ensure_worktree(&pool, repo_id, &wt_path, &slug.value, branch.as_deref())
            .await?;

    let mut sigint = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::interrupt())?;
    loop {
        tokio::select! {
            _ = sigint.recv() => { println!("\nstopping watcher"); break; }
            ev = rx.recv() => match ev {
                Some((framework, dispatch)) => {
                    println!("[{framework}] {dispatch:?}");
                    let payload = serde_json::json!({
                        "framework": framework, "dispatch": dispatch,
                    }).to_string();
                    let _ = treeman_store::write_event(
                        &pool, "info", "watcher_event",
                        Some(&format!("{framework}: {:?}", dispatch)),
                        Some(repo_id), Some(wt_id), None, None, &payload,
                    ).await;
                    if !matches!(dispatch, treeman_watcher::Dispatch::Noop) {
                        let cfg = treeman_core::config::load_layered(Some(&repo_root))?;
                        let env = capture_inherited_env();
                        let res: Result<(), anyhow::Error> = match &dispatch {
                            treeman_watcher::Dispatch::Delta(_) => {
                                match treeman_prepare::delta_run(
                                    &cfg, &repo_root, &slug, &pool, repo_id, wt_id, &env,
                                ).await {
                                    Ok(outs) => {
                                        for o in outs {
                                            let label = paint("delta", Style::new().green());
                                            println!("  {label} [{}] src={} clones={}",
                                                o.engine, o.source_db, o.clones.len());
                                        }
                                        Ok(())
                                    }
                                    Err(e) => Err(e),
                                }
                            }
                            _ => {
                                match treeman_prepare::run(
                                    &cfg, &repo_root, &slug, &pool, repo_id, wt_id, &env,
                                ).await {
                                    Ok(outs) => {
                                        for o in outs {
                                            let label = if o.cache_hit {
                                                paint("cache_hit", Style::new().cyan())
                                            } else {
                                                paint("rebuilt", Style::new().yellow())
                                            };
                                            println!("  prepare[{}] src={} ({label})", o.engine, o.source_db);
                                        }
                                        Ok(())
                                    }
                                    Err(e) => Err(e),
                                }
                            }
                        };
                        if let Err(e) = res {
                            eprintln!("prepare error: {e:#}");
                            let _ = treeman_store::write_event(
                                &pool, "error", "prepare_error",
                                Some(&e.to_string()),
                                Some(repo_id), Some(wt_id), None, None, "{}",
                            ).await;
                        }
                    }
                }
                None => break,
            }
        }
    }
    Ok(())
}

// ───────────────────────── snapshot ─────────────────────────

async fn snapshot_list(engine: Option<String>) -> Result<()> {
    let pool = open_pool().await?;
    let rows = treeman_snapshot::list(&pool, engine.as_deref()).await?;
    if rows.is_empty() {
        println!("(no snapshots recorded)");
        return Ok(());
    }
    let hdr = paint(
        &format!(
            "{:<18} {:<10} {:<22} {:<10} {}",
            "FINGERPRINT", "ENGINE", "SOURCE_DB", "USES", "LAST_USED"
        ),
        Style::new().bold(),
    );
    println!("{hdr}");
    for r in rows {
        let ts = chrono::DateTime::from_timestamp_millis(r.last_used_at)
            .map(|t| t.format("%Y-%m-%d %H:%M").to_string())
            .unwrap_or_default();
        println!(
            "{:<18} {:<10} {:<22} {:<10} {ts}",
            &r.fingerprint[..16.min(r.fingerprint.len())],
            r.engine,
            r.source_db,
            r.use_count
        );
    }
    Ok(())
}

async fn snapshot_show(fingerprint: String) -> Result<()> {
    let pool = open_pool().await?;
    let rows = treeman_snapshot::list(&pool, None).await?;
    let row = rows
        .into_iter()
        .find(|r| r.fingerprint.starts_with(&fingerprint))
        .with_context(|| format!("snapshot not found: {fingerprint}"))?;
    println!("{}", serde_json::to_string_pretty(&row)?);
    Ok(())
}

async fn snapshot_gc(keep: u32, max_age_days: u32, max_total_gb: u32) -> Result<()> {
    let pool = open_pool().await?;
    let dropped = treeman_snapshot::gc_lru(&pool, keep, max_age_days, max_total_gb).await?;
    if dropped.is_empty() {
        println!("(nothing to gc)");
    } else {
        println!("dropped {} snapshot catalog row(s):", dropped.len());
        for r in dropped {
            println!(
                "  {} ({}) template={}",
                &r.fingerprint[..16],
                r.engine,
                r.template_name
            );
        }
        println!("note: engine-side DROP DATABASE is up to the caller (run `treeman db drop`)");
    }
    Ok(())
}

// ───────────────────────── prepare ─────────────────────────

async fn prepare_cmd(args: PrepareArgs) -> Result<()> {
    let wt_path = match args.worktree.worktree {
        Some(p) => p.canonicalize()?,
        None => std::env::current_dir()?,
    };
    let repo_root = match args.repo.repo {
        Some(r) => r.canonicalize()?,
        None => discover_repo_root(&wt_path).context("no repo root")?,
    };
    let cfg = treeman_core::config::load_layered(Some(&repo_root))?;
    let branch = detect_branch(&wt_path);
    let slug = treeman_core::slug_for(&wt_path, branch.as_deref());

    let pool = open_pool().await?;
    let repo_name = repo_root
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("repo");
    let repo_id = treeman_store::ensure_repo(&pool, &repo_root, repo_name).await?;
    let wt_id =
        treeman_store::ensure_worktree(&pool, repo_id, &wt_path, &slug.value, branch.as_deref())
            .await?;

    let env = capture_inherited_env();
    let outcomes =
        treeman_prepare::run(&cfg, &repo_root, &slug, &pool, repo_id, wt_id, &env).await?;
    for o in outcomes {
        let label = if o.cache_hit {
            paint("cache_hit", Style::new().cyan().bold())
        } else {
            paint("cold_build", Style::new().yellow().bold())
        };
        println!(
            "[{}] {} src={} template={} ({} clones)",
            o.engine,
            label,
            o.source_db,
            o.template_name,
            o.clones.len()
        );
        for c in &o.clones {
            println!("  → {c}");
        }
    }
    Ok(())
}

// ───────────────────────── worktree create/delete ─────────────────────────

async fn worktree_finalize(path: PathBuf, repo: Option<PathBuf>) -> Result<()> {
    let wt_path = path
        .canonicalize()
        .with_context(|| format!("canonicalize {} — does the worktree exist?", path.display()))?;
    let repo_root = match repo {
        Some(r) => r.canonicalize()?,
        None => discover_repo_root(&wt_path).context("could not find repo root for worktree")?,
    };
    send_finalize_to_daemon(&repo_root, &wt_path, capture_inherited_env()).await?;
    println!(
        "{} postcreate + prepare detached to daemon — \
         follow with `treeman logs tail -f`",
        paint("queued:", style_ok())
    );
    Ok(())
}

/// Snapshot the calling process's env. The daemon `env_clear()`s
/// before spawning hook + migrate subprocesses and replaces with
/// this map so they see the caller's `$PATH` rather than the
/// daemon's minimal systemd-user env.
fn capture_inherited_env() -> std::collections::BTreeMap<String, String> {
    std::env::vars().collect()
}

/// Ask the daemon to take ownership of postcreate+prepare for `wt`.
/// Returns once the daemon has acknowledged — the actual hook + DB
/// work runs in the daemon's runtime and is not awaited here.
async fn send_finalize_to_daemon(
    repo_root: &Path,
    wt_path: &Path,
    inherited_env: std::collections::BTreeMap<String, String>,
) -> Result<()> {
    let req = treeman_proto::Request::WorktreeFinalize {
        repo_path: repo_root.to_string_lossy().into_owned(),
        worktree_path: wt_path.to_string_lossy().into_owned(),
        inherited_env,
    };
    match crate::client::call(req).await? {
        treeman_proto::Response::WorktreeFinalizeQueued { .. } => Ok(()),
        treeman_proto::Response::Error { message } => bail!("daemon: {message}"),
        other => bail!("unexpected daemon response: {other:?}"),
    }
}

/// Same shape as [`send_finalize_to_daemon`] but for `wt delete`. The
/// daemon runs predelete hooks + DB teardown + `git worktree remove`
/// in its tokio runtime so the calling shell returns immediately.
async fn send_teardown_to_daemon(
    repo_root: &Path,
    wt_path: &Path,
    force: bool,
    inherited_env: std::collections::BTreeMap<String, String>,
) -> Result<()> {
    let req = treeman_proto::Request::WorktreeTeardown {
        repo_path: repo_root.to_string_lossy().into_owned(),
        worktree_path: wt_path.to_string_lossy().into_owned(),
        force,
        inherited_env,
    };
    match crate::client::call(req).await? {
        treeman_proto::Response::WorktreeTeardownQueued { .. } => Ok(()),
        treeman_proto::Response::Error { message } => bail!("daemon: {message}"),
        other => bail!("unexpected daemon response: {other:?}"),
    }
}

async fn worktree_create(
    branch: String,
    from: Option<String>,
    path: Option<PathBuf>,
    repo: Option<PathBuf>,
    skip_hooks: bool,
    skip_prepare: bool,
    foreground: bool,
) -> Result<()> {
    use treeman_core::template::{TemplateContext, render};

    let repo_root = match repo {
        Some(r) => r.canonicalize()?,
        None => discover_repo_root(&std::env::current_dir()?).context("no repo root found")?,
    };
    let cfg = treeman_core::config::load_layered(Some(&repo_root))?;

    let wt_path = match path {
        Some(p) => {
            if p.is_absolute() {
                p
            } else {
                repo_root.join(p)
            }
        }
        None => {
            let root = resolve_worktrees_root(&cfg, &repo_root);
            root.join(&branch)
        }
    };
    if wt_path.exists() {
        bail!("destination path already exists: {}", wt_path.display());
    }
    ensure_not_nested_worktree(&repo_root, &wt_path)?;
    let _ = check_worktrees_gitignored(&cfg, &repo_root);
    std::fs::create_dir_all(wt_path.parent().unwrap_or(&repo_root))?;

    let base = match from {
        Some(b) => b,
        None => detect_default_branch(&repo_root)?,
    };
    let branch_exists = std::process::Command::new("git")
        .arg("-C")
        .arg(&repo_root)
        .arg("rev-parse")
        .arg("--verify")
        .arg("--quiet")
        .arg(format!("refs/heads/{branch}"))
        .status()
        .map(|s| s.success())
        .unwrap_or(false);
    let status = if branch_exists {
        std::process::Command::new("git")
            .arg("-C")
            .arg(&repo_root)
            .arg("worktree")
            .arg("add")
            .arg(&wt_path)
            .arg(&branch)
            .status()?
    } else {
        std::process::Command::new("git")
            .arg("-C")
            .arg(&repo_root)
            .arg("worktree")
            .arg("add")
            .arg("-b")
            .arg(&branch)
            .arg(&wt_path)
            .arg(&base)
            .status()?
    };
    if !status.success() {
        bail!("git worktree add failed");
    }
    let wt_path = wt_path
        .canonicalize()
        .with_context(|| format!("canonicalize new worktree {}", wt_path.display()))?;

    for rel in &cfg.worktrees.links {
        let src = repo_root.join(rel);
        let dst = wt_path.join(rel);
        if !src.exists() {
            eprintln!("warn: link source missing, skipping: {}", src.display());
            continue;
        }
        if dst.exists() {
            continue;
        }
        if let Some(parent) = dst.parent() {
            std::fs::create_dir_all(parent).ok();
        }
        std::os::unix::fs::symlink(&src, &dst)
            .with_context(|| format!("symlink {} → {}", dst.display(), src.display()))?;
    }

    let slug = treeman_core::slug_for(&wt_path, Some(&branch));
    let ctx = TemplateContext::from_slug(&slug);
    let env_files: Vec<PathBuf> = cfg
        .env_scoping
        .files
        .iter()
        .map(|f| wt_path.join(f))
        .collect();
    let pairs: Vec<(String, String)> = cfg
        .env_scoping
        .patches
        .iter()
        .map(|p| render(&p.template, &ctx).map(|v| (p.key.clone(), v)))
        .collect::<Result<Vec<_>, _>>()?;
    for f in &env_files {
        if !f.exists() {
            continue;
        }
        let is_xml = f.extension().and_then(|s| s.to_str()) == Some("xml");
        let outcome = if is_xml {
            treeman_core::patcher::patch_phpunit_file(f, &pairs)?
        } else {
            treeman_core::patcher::patch_env_file(f, &pairs)?
        };
        if matches!(outcome, treeman_core::patcher::PatchOutcome::Updated) {
            println!("patched {}", f.display());
            if cfg.env_scoping.skip_worktree {
                let _ = treeman_core::patcher::skip_worktree(&wt_path, f);
            }
        }
    }

    let pool = open_pool().await?;
    let repo_name = repo_root
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("repo");
    let repo_id = treeman_store::ensure_repo(&pool, &repo_root, repo_name).await?;
    let wt_id =
        treeman_store::ensure_worktree(&pool, repo_id, &wt_path, &slug.value, Some(&branch))
            .await?;
    let ok = paint("ok", Style::new().green().bold());
    println!(
        "{ok} created worktree #{wt_id} slug={} path={}",
        slug.value,
        wt_path.display()
    );

    if skip_hooks {
        return Ok(());
    }

    let env = capture_inherited_env();

    // precreate is always synchronous — it may legitimately need to
    // succeed before the worktree is considered usable (e.g. fetch a
    // submodule into the worktree).
    if !cfg.hooks.precreate.is_empty() {
        let outcome = treeman_core::hooks::run_precreate_hooks(
            &cfg.hooks.precreate,
            &repo_root,
            &wt_path,
            &slug.value,
            &env,
        )
        .await?;
        println!("precreate: exit={}", outcome.aggregate_exit_code);
        if outcome.aggregate_exit_code != 0 {
            bail!("precreate failed");
        }
    }

    // Async-create gate. The slow tail (postcreate hooks + prepare)
    // gets handed off to the daemon when `worktrees.async_create` is
    // true and the user didn't pass `--foreground`. The CLI returns
    // immediately and the daemon's tokio runtime owns the work.
    let should_async = cfg.worktrees.async_create
        && !foreground
        && !skip_prepare
        && (!cfg.hooks.postcreate.is_empty() || !cfg.databases.is_empty());
    if should_async {
        match send_finalize_to_daemon(&repo_root, &wt_path, env.clone()).await {
            Ok(()) => {
                println!(
                    "{} postcreate + prepare detached to daemon — \
                     follow with `treeman logs tail -f`",
                    paint("queued:", style_ok())
                );
                return Ok(());
            }
            Err(e) => {
                eprintln!("warn: daemon RPC failed ({e}); falling back to foreground");
            }
        }
    }

    if !cfg.hooks.postcreate.is_empty() {
        let outcome = treeman_core::hooks::run_hooks(
            "postcreate",
            &cfg.hooks.postcreate,
            &repo_root,
            &wt_path,
            &slug.value,
            &env,
        )
        .await?;
        println!(
            "postcreate: {} group(s) spawned (logs in {}/.treeman-hooks/)",
            outcome.groups.len(),
            wt_path.display()
        );
    }
    if !skip_prepare && !cfg.databases.is_empty() {
        match treeman_prepare::run(&cfg, &repo_root, &slug, &pool, repo_id, wt_id, &env).await {
            Ok(outs) => {
                for o in outs {
                    println!(
                        "prepare[{}] {} ({})",
                        o.engine,
                        o.source_db,
                        if o.cache_hit { "cache_hit" } else { "rebuilt" }
                    );
                }
            }
            Err(e) => eprintln!("warn: prepare failed: {e:#}"),
        }
    }
    Ok(())
}

async fn worktree_delete(
    target: String,
    repo: Option<PathBuf>,
    force: bool,
    foreground: bool,
) -> Result<()> {
    let pool = open_pool().await?;
    let wt = if let Ok(p) = PathBuf::from(&target).canonicalize() {
        treeman_store::hook_runs::find_worktree_by_path(&pool, &p.to_string_lossy()).await?
    } else {
        None
    };
    let wt = match wt {
        Some(w) => w,
        None => {
            let rows = treeman_store::hook_runs::list_worktrees(&pool).await?;
            rows.into_iter()
                .find(|r| r.branch.as_deref() == Some(target.as_str()))
                .with_context(|| format!("worktree not found: {target}"))?
        }
    };
    let wt_path = PathBuf::from(&wt.path);
    let repo_root = match repo {
        Some(r) => r.canonicalize()?,
        None => discover_repo_root(&wt_path).context("no repo root for worktree")?,
    };
    let cfg = treeman_core::config::load_layered(Some(&repo_root))?;
    let slug = treeman_core::slug::Slug {
        value: wt.slug.clone(),
        source: treeman_core::slug::SlugSource::Ticket,
    };

    let env = capture_inherited_env();

    // Async-delete gate. Mirror of async_create: hand predelete +
    // DB teardown + git worktree remove to the daemon and return
    // immediately.
    if cfg.worktrees.async_delete && !foreground {
        match send_teardown_to_daemon(&repo_root, &wt_path, force, env.clone()).await {
            Ok(()) => {
                println!(
                    "{} predelete + DB teardown + git remove detached to \
                     daemon — follow with `treeman logs tail -f`",
                    paint("queued:", style_ok())
                );
                return Ok(());
            }
            Err(e) => {
                eprintln!("warn: daemon RPC failed ({e}); falling back to foreground");
            }
        }
    }

    if !cfg.hooks.predelete.is_empty() {
        let outcome = treeman_core::hooks::run_hooks(
            "predelete",
            &cfg.hooks.predelete,
            &repo_root,
            &wt_path,
            &slug.value,
            &env,
        )
        .await?;
        println!(
            "predelete: {} group(s) spawned (logs in {}/.treeman-hooks/)",
            outcome.groups.len(),
            wt_path.display()
        );
    }
    teardown_databases(&cfg, &slug.value, wt.repo_id, wt.id, &pool).await?;

    let mut args = vec!["worktree".to_string(), "remove".into()];
    if force {
        args.push("--force".into());
    }
    args.push(wt_path.to_string_lossy().to_string());
    let status = std::process::Command::new("git")
        .arg("-C")
        .arg(&repo_root)
        .args(&args)
        .status()?;
    if !status.success() && !force {
        bail!("git worktree remove failed; pass --force to override");
    }

    treeman_store::mark_worktree_deleted(&pool, wt.id).await?;
    prune_empty_parents(&wt_path, &resolve_worktrees_root(&cfg, &repo_root));
    println!(
        "{} deleted worktree {} ({})",
        paint("ok:", style_ok()),
        paint(&format!("#{}", wt.id), style_count()),
        paint(&wt_path.display().to_string(), style_path())
    );
    Ok(())
}

/// Hint the user (once, on stderr) when `worktrees.root` resolves
/// inside the repo and the linked-worktree dir isn't excluded by
/// `.gitignore`. We deliberately do NOT mutate `.gitignore` — that's
/// the user's file and treeman has no business rewriting it.
fn check_worktrees_gitignored(cfg: &treeman_core::config::Config, repo_root: &Path) -> Result<()> {
    let root = resolve_worktrees_root(cfg, repo_root);
    let root_canon = root.canonicalize().unwrap_or_else(|_| root.clone());
    let repo_canon = repo_root
        .canonicalize()
        .unwrap_or_else(|_| repo_root.to_path_buf());
    let Ok(rel) = root_canon.strip_prefix(&repo_canon) else {
        return Ok(()); // root lives outside the repo — nothing to ignore
    };
    let rel_str = rel.to_string_lossy().replace('\\', "/");
    if rel_str.is_empty() {
        return Ok(());
    }
    let entry = format!("/{rel_str}/");
    let gitignore = repo_root.join(".gitignore");
    let existing = std::fs::read_to_string(&gitignore).unwrap_or_default();
    let already = existing.lines().any(|l| {
        let t = l.trim();
        t == entry || t == format!("/{rel_str}") || t == rel_str || t == format!("{rel_str}/")
    });
    if already {
        return Ok(());
    }
    eprintln!(
        "hint: worktrees.root resolves to '{rel_str}/' inside this repo; \
         consider adding `/{rel_str}/` to .gitignore so linked worktrees \
         don't show up as untracked content"
    );
    Ok(())
}

fn resolve_worktrees_root(cfg: &treeman_core::config::Config, repo_root: &Path) -> PathBuf {
    let raw = &cfg.worktrees.root;
    if Path::new(raw).is_absolute() {
        PathBuf::from(raw)
    } else {
        repo_root.join(raw)
    }
}

/// Walk up from `wt_path` and `rmdir` each empty parent. Stops at
/// `worktrees_root` (exclusive) so we never delete the configured root or
/// anything outside it. Best-effort: any non-empty dir or `remove_dir`
/// failure aborts the walk.
fn prune_empty_parents(wt_path: &Path, worktrees_root: &Path) {
    let stop = worktrees_root
        .canonicalize()
        .unwrap_or_else(|_| worktrees_root.to_path_buf());
    let mut dir = match wt_path.parent() {
        Some(p) => p.to_path_buf(),
        None => return,
    };
    loop {
        let canon = dir.canonicalize().unwrap_or_else(|_| dir.clone());
        if canon == stop {
            return;
        }
        if !canon.starts_with(&stop) {
            return;
        }
        match std::fs::read_dir(&dir) {
            Ok(mut rd) => {
                if rd.next().is_some() {
                    return; // not empty
                }
            }
            Err(_) => return,
        }
        if std::fs::remove_dir(&dir).is_err() {
            return;
        }
        match dir.parent() {
            Some(p) => dir = p.to_path_buf(),
            None => return,
        }
    }
}

fn detect_default_branch(repo_root: &Path) -> Result<String> {
    let out = std::process::Command::new("git")
        .arg("-C")
        .arg(repo_root)
        .arg("symbolic-ref")
        .arg("--short")
        .arg("refs/remotes/origin/HEAD")
        .output()?;
    if out.status.success() {
        let s = String::from_utf8_lossy(&out.stdout).trim().to_string();
        if let Some(b) = s.strip_prefix("origin/") {
            return Ok(b.to_string());
        }
    }
    let out = std::process::Command::new("git")
        .arg("-C")
        .arg(repo_root)
        .arg("rev-parse")
        .arg("--abbrev-ref")
        .arg("HEAD")
        .output()?;
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

// ───────────────────────── init ─────────────────────────

/// Look for a JS package-manager lockfile in `repo_root` and return a
/// hook fragment (leading newline + indented YAML line) that installs
/// deps in the background. Also detects a `frontend/` sub-app with its
/// own lockfile and emits a second commented-out hook for it (so the
/// user can uncomment if they really do install both halves).
///
/// Returns "" when no lockfile is present — the resulting preset has
/// just the composer step and nothing else.
fn detect_js_pkg_mgr_hook(repo_root: &Path) -> String {
    fn detect(dir: &Path) -> Option<&'static str> {
        // Order matters: a repo can carry stale lockfiles after a
        // migration, so prefer the most specific one. deno > bun >
        // pnpm > yarn > npm is a reasonable specificity ordering since
        // each newer tool tends to be added deliberately.
        //
        // Deno uses `deno.lock` (since 1.34) and is identified by
        // `deno.json`/`deno.jsonc` even without a lockfile. We require
        // the manifest to avoid false positives in repos that just
        // happen to commit a stray deno.lock from tooling.
        if (dir.join("deno.json").exists() || dir.join("deno.jsonc").exists())
            && dir.join("deno.lock").exists()
        {
            Some("deno install --frozen")
        } else if dir.join("bun.lockb").exists() || dir.join("bun.lock").exists() {
            Some("bun install")
        } else if dir.join("pnpm-lock.yaml").exists() {
            Some("pnpm install --frozen-lockfile")
        } else if dir.join("yarn.lock").exists() {
            Some("yarn install --frozen-lockfile")
        } else if dir.join("package-lock.json").exists() {
            Some("npm ci")
        } else {
            None
        }
    }

    let root_cmd = detect(repo_root);
    let frontend_cmd = detect(&repo_root.join("frontend"));

    let mut out = String::new();
    if let Some(cmd) = root_cmd {
        out.push_str(&format!(
            "\n    # JS deps are independent of PHP — install in background.\n    - {{ run: \"{cmd}\", background: true }}"
        ));
    }
    if let Some(cmd) = frontend_cmd {
        // Always commented for sub-app — only uncomment if the repo
        // actually needs both root + frontend installs.
        out.push_str(&format!(
            "\n    # - {{ run: \"{cmd}\", cwd: frontend, background: true }}"
        ));
    }
    out
}

fn schema_url_for_repo() -> String {
    if let Ok(home) = std::env::var("HOME") {
        let xdg = std::env::var("XDG_CONFIG_HOME").unwrap_or_else(|_| format!("{home}/.config"));
        let path = PathBuf::from(xdg).join("treeman/schemas/repo.schema.json");
        return format!("file://{}", path.display());
    }
    "file:///etc/treeman/schemas/repo.schema.json".into()
}

fn init_cmd(args: InitArgs) -> Result<()> {
    let repo_root = match args.repo.repo {
        Some(p) => p.canonicalize()?,
        None => discover_repo_root(&std::env::current_dir()?).context("no repo root")?,
    };
    let target = repo_root.join(".treeman.yaml");
    if target.exists() && !args.force {
        bail!(
            "{} already exists (pass --force to overwrite)",
            target.display()
        );
    }
    let registry = treeman_migrations::Registry::with_builtins();
    let detected = registry.detect_all(&repo_root);
    let mut framework_hint = "none";
    let mut engine_hint = "mysql";
    if let Some(s) = detected.first() {
        framework_hint = s.name.as_str();
        if let Some(e) = &s.engine_hint {
            engine_hint = e.as_str();
        }
    }
    let repo_name = repo_root
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("repo");
    let schema_url = schema_url_for_repo();
    // Detect a JS package manager from lockfiles in the repo root.
    // None if the repo has no JS deps at all (so the preset doesn't
    // bake in a yarn step that won't apply).
    let pkg_mgr_hook = detect_js_pkg_mgr_hook(&repo_root);
    let content = if framework_hint == "laravel" {
        format!(
            r#"# yaml-language-server: $schema={schema_url}
# Generated by `treeman init` (Laravel preset). Trim / extend to taste.
# Tip: run `treeman schema install` once to materialize the schema files
# so the modeline above lights up YAML completions in your editor.
#
# Template keys available in env_scoping.patches and *_template fields:
#   {{slug}}              — e.g. proj_1234 (from PROJ-1234 branch/dir)
#   {{slug_dash}}         — e.g. proj-1234 (S3/minio-safe, no underscores)
#   {{slug_redis_queue}}  — redis db index 6..15, deterministic from slug
#   {{slug_redis_cache}}  — second redis db index, distinct from queue
#   {{n}}                 — replica index, only valid inside paratest.name_template
repo:
  name: {repo_name}
worktrees:
  root: ../{repo_name}-worktrees
  links:
    - .env
    - .env.testing
env_scoping:
  files:
    - .env.testing
    - phpunit.xml          # phpunit's <env> block populates $_ENV before bootstrap
  skip_worktree: true
  patches:
    - {{ key: DB_DATABASE,           template: "{repo_name}_testing_{{slug}}" }}
    - {{ key: DB_TEST_DATABASE,      template: "{repo_name}_testing_{{slug}}" }}
    - {{ key: REDIS_QUEUE_DATABASE,  template: "{{slug_redis_queue}}" }}
    - {{ key: REDIS_CACHE_DATABASE,  template: "{{slug_redis_cache}}" }}
    # Common multi-engine extras — uncomment the ones your repo uses:
    # - {{ key: MONGO_DB_DATABASE,      template: "mongodb_testing_{{slug}}" }}
    # - {{ key: OPERATION_LOG_DATABASE, template: "operation_logs_testing_{{slug}}" }}
    # - {{ key: ELASTICSEARCH_PREFIX,   template: "{repo_name}_testing_{{slug}}_" }}
    # - {{ key: STORAGE_PREFIX,         template: "phpunit-{{slug_dash}}-" }}
databases:
  - engine: mysql
    name_template: "{repo_name}_testing_{{slug}}"
    migrations: {{ framework: laravel }}
    # `clones: auto` matches the detected test framework (paratest =
    # runtime.NumCPU, pest = 1, plain phpunit = shared DB only).
    paratest: {{ clones: auto, name_template: "{repo_name}_testing_{{slug}}_test_{{n}}" }}
    # Uncomment to seed from a SQL dump checked into the repo:
    # dump: {{ path: dbdumps/seed.sql }}
  # Uncomment the engines this repo actually uses:
  # - engine: mongodb
  #   name_template: "mongodb_testing_{{slug}}"
  # - engine: redis
  #   namespaces: {{ db_index_template: "{{slug_redis_queue}}" }}
  # - engine: redis
  #   namespaces: {{ db_index_template: "{{slug_redis_cache}}" }}
  # - engine: elasticsearch
  #   namespaces: {{ index_prefix_template: "{repo_name}_testing_{{slug}}_" }}
hooks:
  postcreate:
    # composer install must finish before any artisan command runs.
    - {{ run: "composer install --no-interaction" }}{pkg_mgr_hook}
  predelete: []
watcher:
  paths:
    - {{ glob: "database/migrations/**", on: auto }}
  debounce_ms: 500
"#
        )
    } else {
        format!(
            r#"# yaml-language-server: $schema={schema_url}
# Generated by `treeman init`. Trim / extend to taste.
# Tip: run `treeman schema install` once to materialize the schema files
# so the modeline above lights up YAML completions in your editor.
repo:
  name: {repo_name}
worktrees:
  root: ../{repo_name}-worktrees
  links:
    - .env
    - .env.testing
env_scoping:
  files:
    - .env.testing
  skip_worktree: true
  patches:
    - {{ key: DB_TEST_DATABASE, template: "{repo_name}_testing_{{slug}}" }}
databases:
  - engine: {engine_hint}
    name_template: "{repo_name}_testing_{{slug}}"
    migrations: {{ framework: {framework_hint} }}
    paratest: {{ clones: auto, name_template: "{repo_name}_testing_{{slug}}_test_{{n}}" }}
hooks:
  postcreate: []
  predelete: []
watcher:
  paths:
    - {{ glob: "database/migrations/**", on: auto }}
  debounce_ms: 500
"#
        )
    };
    std::fs::write(&target, content)?;
    println!(
        "wrote {} (detected framework: {framework_hint}, engine: {engine_hint})",
        target.display()
    );
    Ok(())
}

// ───────────────────────── shared helpers ─────────────────────────

// `teardown_databases` was moved to `treeman_prepare::teardown` so the
// daemon's WorktreeTeardown handler can call it without depending on
// the CLI crate. This module re-exports it as `teardown_databases`
// for the local call sites.
use treeman_prepare::teardown_databases;

fn detect_branch(worktree: &Path) -> Option<String> {
    let head = worktree.join(".git/HEAD");
    let head_path = if head.is_file() {
        head
    } else {
        let raw = std::fs::read_to_string(worktree.join(".git")).ok()?;
        let gitdir = raw.trim_start_matches("gitdir:").trim();
        PathBuf::from(gitdir).join("HEAD")
    };
    let raw = std::fs::read_to_string(&head_path).ok()?;
    raw.trim()
        .strip_prefix("ref: refs/heads/")
        .map(|s| s.to_string())
}

fn discover_repo_root(start: &Path) -> Option<PathBuf> {
    let mut dir = start;
    loop {
        let dot_git = dir.join(".git");
        if dot_git.is_dir() {
            return Some(dir.to_path_buf());
        }
        if dot_git.is_file() {
            let raw = std::fs::read_to_string(&dot_git).ok()?;
            let gd = raw.trim_start_matches("gitdir:").trim();
            let common = PathBuf::from(gd).join("commondir");
            if let Ok(rel) = std::fs::read_to_string(&common) {
                let gitdir = PathBuf::from(gd);
                let common_dir = gitdir.join(rel.trim());
                return common_dir
                    .canonicalize()
                    .ok()
                    .and_then(|p| p.parent().map(|p| p.to_path_buf()));
            }
            return Some(dir.to_path_buf());
        }
        dir = dir.parent()?;
    }
}

/// Reject worktree creation paths that would land inside the main repo's
/// `.git` directory or inside any already-registered linked worktree. Prevents
/// nested worktrees, which `git` does not support and which corrupt state.
fn ensure_not_nested_worktree(repo_root: &Path, wt_path: &Path) -> Result<()> {
    let anchor = wt_path
        .ancestors()
        .find(|p| p.exists())
        .map(|p| p.to_path_buf())
        .unwrap_or_else(|| repo_root.to_path_buf());
    let anchor_abs = anchor.canonicalize().unwrap_or(anchor);

    let git_dir = repo_root.join(".git");
    if let Ok(git_dir_abs) = git_dir.canonicalize() {
        if anchor_abs.starts_with(&git_dir_abs) {
            bail!(
                "refusing to create worktree inside .git: {}",
                wt_path.display()
            );
        }
    }

    let out = std::process::Command::new("git")
        .arg("-C")
        .arg(repo_root)
        .arg("worktree")
        .arg("list")
        .arg("--porcelain")
        .output()
        .context("running `git worktree list --porcelain`")?;
    if !out.status.success() {
        bail!(
            "`git worktree list` failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
    }
    let stdout = String::from_utf8_lossy(&out.stdout);
    let main_root_abs = repo_root
        .canonicalize()
        .unwrap_or_else(|_| repo_root.to_path_buf());
    for line in stdout.lines() {
        let Some(p) = line.strip_prefix("worktree ") else {
            continue;
        };
        let existing = PathBuf::from(p);
        let existing_abs = existing.canonicalize().unwrap_or(existing);
        // The main worktree is allowed to contain the worktrees root
        // (e.g. `.worktrees/` lives inside the main repo). Skip it.
        if existing_abs == main_root_abs {
            continue;
        }
        if anchor_abs.starts_with(&existing_abs) {
            bail!(
                "refusing to create worktree nested inside existing worktree {}",
                existing_abs.display()
            );
        }
    }
    Ok(())
}

async fn open_pool() -> Result<sqlx::SqlitePool> {
    let p = treeman_store::default_db_path()?;
    treeman_store::open(&p).await
}
