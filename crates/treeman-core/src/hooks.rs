//! Execute the `hooks.<phase>` steps from a loaded Config.
//!
//! Each step runs as a subshell (`/bin/sh -c "<run>"`) with `GWT_MAIN` /
//! `GWT_WT` env vars set for shim compatibility with the existing
//! `.gwt-postcreate` / `.gwt-predelete` flow.

use std::path::Path;
use std::process::Stdio;

use anyhow::{Context, Result};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Command;

use crate::config::HookStep;

#[derive(Debug, Clone)]
pub struct HookRunOutcome {
    pub steps: Vec<StepOutcome>,
    pub aggregate_exit_code: i32,
}

#[derive(Debug, Clone)]
pub struct StepOutcome {
    pub command: String,
    pub exit_code: i32,
    pub stdout_tail: String,
    pub stderr_tail: String,
    pub background: bool,
}

/// Run all `steps` in order. Background steps are spawned and detached
/// (no wait). The first foreground non-zero exit short-circuits the
/// remaining foreground steps but background steps already spawned keep
/// running.
pub async fn run_hooks(
    steps: &[HookStep],
    repo_root: &Path,
    worktree_path: &Path,
    slug: &str,
) -> Result<HookRunOutcome> {
    let mut outcomes = Vec::with_capacity(steps.len());
    let mut aggregate: i32 = 0;
    for step in steps {
        let cwd = step.cwd.as_deref().unwrap_or(worktree_path);
        let cmd_str = step.run.clone();
        if step.background {
            spawn_background(&cmd_str, cwd, repo_root, worktree_path, slug)?;
            outcomes.push(StepOutcome {
                command: cmd_str,
                exit_code: 0,
                stdout_tail: String::new(),
                stderr_tail: String::new(),
                background: true,
            });
            continue;
        }
        if aggregate != 0 {
            // Short-circuit; record skipped step.
            outcomes.push(StepOutcome {
                command: cmd_str,
                exit_code: -1,
                stdout_tail: String::new(),
                stderr_tail: "skipped (prior step failed)".into(),
                background: false,
            });
            continue;
        }
        let out = run_foreground(&cmd_str, cwd, repo_root, worktree_path, slug).await?;
        if out.exit_code != 0 && aggregate == 0 {
            aggregate = out.exit_code;
        }
        outcomes.push(out);
    }
    Ok(HookRunOutcome { steps: outcomes, aggregate_exit_code: aggregate })
}

async fn run_foreground(
    cmd: &str,
    cwd: &Path,
    repo_root: &Path,
    worktree_path: &Path,
    slug: &str,
) -> Result<StepOutcome> {
    let mut child = Command::new("/bin/sh")
        .arg("-c").arg(cmd)
        .current_dir(cwd)
        .env("GWT_MAIN", repo_root)
        .env("GWT_WT", worktree_path)
        .env("TREEMAN_SLUG", slug)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .with_context(|| format!("spawn `{cmd}`"))?;

    // Capture tails (last 16 KiB) so we don't blow up the events table.
    let stdout = child.stdout.take().expect("piped");
    let stderr = child.stderr.take().expect("piped");
    let stdout_task = tokio::spawn(capture_tail(stdout, 16 * 1024));
    let stderr_task = tokio::spawn(capture_tail(stderr, 16 * 1024));

    let status = child.wait().await.context("wait child")?;
    let stdout_tail = stdout_task.await.unwrap_or_default();
    let stderr_tail = stderr_task.await.unwrap_or_default();

    Ok(StepOutcome {
        command: cmd.to_string(),
        exit_code: status.code().unwrap_or(-1),
        stdout_tail,
        stderr_tail,
        background: false,
    })
}

fn spawn_background(
    cmd: &str,
    cwd: &Path,
    repo_root: &Path,
    worktree_path: &Path,
    slug: &str,
) -> Result<()> {
    // setsid so the background process survives shell exit; mirrors the
    // existing .gwt-postcreate behavior.
    std::process::Command::new("setsid")
        .arg("/bin/sh").arg("-c").arg(cmd)
        .current_dir(cwd)
        .env("GWT_MAIN", repo_root)
        .env("GWT_WT", worktree_path)
        .env("TREEMAN_SLUG", slug)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .with_context(|| format!("setsid spawn `{cmd}`"))?;
    Ok(())
}

async fn capture_tail<R: tokio::io::AsyncRead + Unpin + Send + 'static>(
    r: R,
    cap_bytes: usize,
) -> String {
    let mut reader = BufReader::new(r);
    let mut buf = Vec::with_capacity(cap_bytes.min(8 * 1024));
    let mut line = String::new();
    loop {
        line.clear();
        let n = match reader.read_line(&mut line).await { Ok(n) => n, Err(_) => break };
        if n == 0 { break; }
        buf.extend_from_slice(line.as_bytes());
        if buf.len() > cap_bytes {
            let drop_n = buf.len() - cap_bytes;
            buf.drain(0..drop_n);
        }
    }
    String::from_utf8_lossy(&buf).to_string()
}
