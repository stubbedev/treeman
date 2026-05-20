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
use treeman_db::DbDriver;
use treeman_proto::{Request, Response};
use treeman_store::query::{EventFilter, query_events, tail_events};

// ───────────────────────── value_enums ─────────────────────────

#[derive(Debug, Clone, Copy, ValueEnum)]
#[clap(rename_all = "lower")]
enum EngineArg {
    Mysql,
    Postgres,
    #[clap(alias = "mongo")]
    Mongodb,
    Redis,
    #[clap(alias = "es")]
    Elasticsearch,
    Sqlite,
}

impl EngineArg {
    fn as_str(self) -> &'static str {
        match self {
            EngineArg::Mysql => "mysql",
            EngineArg::Postgres => "postgres",
            EngineArg::Mongodb => "mongodb",
            EngineArg::Redis => "redis",
            EngineArg::Elasticsearch => "elasticsearch",
            EngineArg::Sqlite => "sqlite",
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
  treeman wt create KON-1234         # git worktree add + DB scoping + prepare
  treeman watcher start              # daemon-managed migration file watcher
  treeman wt delete KON-1234         # predelete hook + DB teardown + git worktree remove

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
        }) => worktree_create(branch, from, path, repo.repo, skip_hooks, skip_prepare).await,
        Cmd::Worktree(WorktreeCmd::Delete {
            target,
            repo,
            force,
        }) => worktree_delete(target, repo.repo, force).await,
        Cmd::Worktree(WorktreeCmd::Register { path, branch, repo }) => {
            worktree_register(path, branch, repo.repo).await
        }
        Cmd::Worktree(WorktreeCmd::List) => worktree_list().await,
        Cmd::Worktree(WorktreeCmd::Unregister { path }) => worktree_unregister(path).await,
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
            println!("daemon_version: {}", s.daemon_version);
            println!("protocol:       v{}", s.protocol_version);
            println!("pid:            {}", s.pid);
            println!("started_at:     {}", s.started_at_unix);
            println!("watchers:       {}", s.watcher_count);
        }
        Response::Error { message } => bail!("daemon error: {message}"),
        other => bail!("unexpected response: {:?}", other),
    }
    Ok(())
}

async fn daemon_start() -> Result<()> {
    if let Ok(Response::Pong) = client::call(Request::Ping).await {
        println!("treemand already running");
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
            println!("shutdown sent");
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
        println!("treemand started via launchd ({})", launchd_label());
        Ok(())
    } else {
        let s = std::process::Command::new("systemctl")
            .args(["--user", "start", "treemand.service"])
            .status()
            .context("systemctl --user start")?;
        if !s.success() {
            bail!("systemctl --user start treemand.service failed");
        }
        println!("treemand started via systemd --user");
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
        println!("treemand stopped via launchd");
        Ok(())
    } else {
        let s = std::process::Command::new("systemctl")
            .args(["--user", "stop", "treemand.service"])
            .status()
            .context("systemctl --user stop")?;
        if !s.success() {
            bail!("systemctl --user stop treemand.service failed");
        }
        println!("treemand stopped via systemd --user");
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
    println!("wrote {}", unit_path.display());
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
        println!("removed {}", unit_path.display());
    }
    let _ = std::process::Command::new("systemctl")
        .args(["--user", "daemon-reload"])
        .status();
    if !existed {
        println!("no systemd user unit at {}", unit_path.display());
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
    println!("wrote {}", plist_path.display());
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
        println!("removed {}", plist_path.display());
    } else {
        println!("no LaunchAgent at {}", plist_path.display());
    }
    Ok(())
}

async fn watcher_start(repo: Option<PathBuf>) -> Result<()> {
    let repo_path = resolve_repo_for_watcher(repo)?;
    let req = Request::WatcherStart {
        repo_path: repo_path.to_string_lossy().to_string(),
    };
    match client::call(req).await? {
        Response::WatcherStarted { repo_path } => println!("watcher started: {repo_path}"),
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
        Response::WatcherStopped { repo_path } => println!("watcher stopped: {repo_path}"),
        Response::Error { message } => bail!("daemon: {message}"),
        other => bail!("unexpected: {other:?}"),
    }
    Ok(())
}

async fn watcher_list() -> Result<()> {
    match client::call(Request::WatcherList).await? {
        Response::WatcherList { repos } => {
            if repos.is_empty() {
                println!("(no watchers running)");
            } else {
                for r in repos {
                    println!("{r}");
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
        let r = resolve(&cfg, repo.as_deref());
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

    println!("wrote {}", global.display());
    println!("wrote {}", repo.display());
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
        println!("(no worktrees registered)");
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
    println!("unregistered worktree #{} ({})", wt.id, wt.path);
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
    let steps = match phase.as_str() {
        "precreate" => &cfg.hooks.precreate,
        "postcreate" => &cfg.hooks.postcreate,
        "predelete" => &cfg.hooks.predelete,
        "postdelete" => &cfg.hooks.postdelete,
        other => bail!("unknown hook phase: {other}"),
    };
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
    let run_id = treeman_store::hook_runs::start_hook_run(&pool, wt_id, &phase).await?;

    let start = std::time::Instant::now();
    let outcome = treeman_core::hooks::run_hooks(steps, &repo_root, &wt_path, &slug.value).await?;
    let duration_ms = start.elapsed().as_millis() as i64;
    let mut stdout = String::new();
    let mut stderr = String::new();
    for (i, s) in outcome.steps.iter().enumerate() {
        let kind = if s.background {
            "background"
        } else {
            "foreground"
        };
        stdout.push_str(&format!("--- step {i} ({kind}) ---\n{}\n", s.stdout_tail));
        if !s.stderr_tail.is_empty() {
            stderr.push_str(&format!("--- step {i} stderr ---\n{}\n", s.stderr_tail));
        }
        let payload = serde_json::json!({
            "command": s.command, "exit_code": s.exit_code, "background": s.background,
            "stdout_tail": s.stdout_tail, "stderr_tail": s.stderr_tail,
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
        "run_id": run_id, "exit_code": outcome.aggregate_exit_code, "step_count": outcome.steps.len()
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
                        match treeman_prepare::run(
                            &cfg, &repo_root, &slug, &pool, repo_id, wt_id,
                        ).await {
                            Ok(outs) => for o in outs {
                                let label = if o.cache_hit {
                                    paint("cache_hit", Style::new().cyan())
                                } else {
                                    paint("rebuilt", Style::new().yellow())
                                };
                                println!("  prepare[{}] src={} ({label})", o.engine, o.source_db);
                            },
                            Err(e) => {
                                eprintln!("prepare error: {e:#}");
                                let _ = treeman_store::write_event(
                                    &pool, "error", "prepare_error",
                                    Some(&e.to_string()),
                                    Some(repo_id), Some(wt_id), None, None, "{}",
                                ).await;
                            }
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

    let outcomes = treeman_prepare::run(&cfg, &repo_root, &slug, &pool, repo_id, wt_id).await?;
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

async fn worktree_create(
    branch: String,
    from: Option<String>,
    path: Option<PathBuf>,
    repo: Option<PathBuf>,
    skip_hooks: bool,
    skip_prepare: bool,
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
            let root = if cfg.worktrees.root.starts_with('/') {
                PathBuf::from(&cfg.worktrees.root)
            } else {
                repo_root.join(&cfg.worktrees.root)
            };
            root.join(branch.replace('/', "-"))
        }
    };
    if wt_path.exists() {
        bail!("destination path already exists: {}", wt_path.display());
    }
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
    for (phase_name, steps) in [
        ("precreate", &cfg.hooks.precreate),
        ("postcreate", &cfg.hooks.postcreate),
    ] {
        if steps.is_empty() {
            continue;
        }
        let outcome =
            treeman_core::hooks::run_hooks(steps, &repo_root, &wt_path, &slug.value).await?;
        println!("{phase_name}: exit={}", outcome.aggregate_exit_code);
        if outcome.aggregate_exit_code != 0 {
            bail!("{phase_name} failed");
        }
    }
    if !skip_prepare && !cfg.databases.is_empty() {
        match treeman_prepare::run(&cfg, &repo_root, &slug, &pool, repo_id, wt_id).await {
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

async fn worktree_delete(target: String, repo: Option<PathBuf>, force: bool) -> Result<()> {
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

    if !cfg.hooks.predelete.is_empty() {
        let outcome =
            treeman_core::hooks::run_hooks(&cfg.hooks.predelete, &repo_root, &wt_path, &slug.value)
                .await?;
        if outcome.aggregate_exit_code != 0 && !force {
            bail!("predelete hook failed; pass --force to delete anyway");
        }
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
    println!("deleted worktree #{} ({})", wt.id, wt_path.display());
    Ok(())
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
    let content = format!(
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
  files: [".env.testing"]
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
    );
    std::fs::write(&target, content)?;
    println!(
        "wrote {} (detected framework: {framework_hint}, engine: {engine_hint})",
        target.display()
    );
    Ok(())
}

// ───────────────────────── shared helpers ─────────────────────────

async fn teardown_databases(
    cfg: &treeman_core::Config,
    slug: &str,
    repo_id: i64,
    wt_id: i64,
    sqlite_pool: &sqlx::SqlitePool,
) -> Result<()> {
    use treeman_core::config::DatabaseConfig as DB;
    use treeman_core::template::{TemplateContext, render};
    use treeman_db::Namespace;

    let ctx = TemplateContext::from_slug(&treeman_core::slug::Slug {
        value: slug.into(),
        source: treeman_core::slug::SlugSource::Ticket,
    });

    for d in &cfg.databases {
        let result: Result<()> = async {
            match d {
                DB::Mysql { name_template, .. } => {
                    let name = render(name_template, &ctx)?;
                    let mc = cfg
                        .connections
                        .mysql
                        .clone()
                        .context("connections.mysql not configured")?;
                    let drv = treeman_db::mysql::MysqlDriver::connect(&mc).await?;
                    let dropped = drv.drop_matching(&name).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "mysql",
                        slug,
                        &name,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Postgres { name_template, .. } => {
                    let name = render(name_template, &ctx)?;
                    let pc = cfg
                        .connections
                        .postgres
                        .clone()
                        .context("connections.postgres not configured")?;
                    let drv = treeman_db::postgres::PostgresDriver::connect(&pc).await?;
                    let dropped = drv.drop_matching(&name).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "postgres",
                        slug,
                        &name,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Mongodb { name_template } => {
                    let name = render(name_template, &ctx)?;
                    let mc = cfg
                        .connections
                        .mongodb
                        .clone()
                        .context("connections.mongodb not configured")?;
                    let drv = treeman_db::mongo::MongoDriver::connect(&mc).await?;
                    let dropped = drv.drop_matching(&name).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "mongodb",
                        slug,
                        &name,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Elasticsearch { namespaces } => {
                    let prefix = render(&namespaces.index_prefix_template, &ctx)?;
                    let ec = cfg
                        .connections
                        .elasticsearch
                        .clone()
                        .context("connections.elasticsearch not configured")?;
                    let drv = treeman_db::elasticsearch::ElasticsearchDriver::connect(&ec)?;
                    let dropped = drv.drop_matching(&prefix).await?;
                    record(
                        sqlite_pool,
                        "db_drop",
                        "elasticsearch",
                        slug,
                        &prefix,
                        dropped.len(),
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
                DB::Redis { namespaces } => {
                    let idx_str = render(&namespaces.db_index_template, &ctx)?;
                    let idx: u8 = idx_str.parse().context("redis db index parse")?;
                    let rc = cfg
                        .connections
                        .redis
                        .clone()
                        .context("connections.redis not configured")?;
                    let drv = treeman_db::redis_driver::RedisDriver::connect(&rc)?;
                    drv.flush_namespace(&Namespace::RedisDb(idx)).await?;
                    record(
                        sqlite_pool,
                        "db_flush",
                        "redis",
                        slug,
                        &format!("db{idx}"),
                        1,
                        repo_id,
                        wt_id,
                    )
                    .await;
                    Ok(())
                }
            }
        }
        .await;
        if let Err(e) = result {
            eprintln!("warn: teardown failed for {:?}: {e}", d);
            let _ = treeman_store::write_event(
                sqlite_pool,
                "warn",
                "db_teardown_error",
                Some(&e.to_string()),
                Some(repo_id),
                Some(wt_id),
                None,
                None,
                "{}",
            )
            .await;
        }
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn record(
    pool: &sqlx::SqlitePool,
    event_type: &str,
    engine: &str,
    slug: &str,
    target: &str,
    count: usize,
    repo_id: i64,
    wt_id: i64,
) {
    let payload = serde_json::json!({
        "engine": engine, "slug": slug, "target": target, "count": count,
    })
    .to_string();
    let _ = treeman_store::write_event(
        pool,
        "info",
        event_type,
        Some(&format!("{engine}: {target} ({count})")),
        Some(repo_id),
        Some(wt_id),
        None,
        None,
        &payload,
    )
    .await;
}

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

async fn open_pool() -> Result<sqlx::SqlitePool> {
    let p = treeman_store::default_db_path()?;
    treeman_store::open(&p).await
}
