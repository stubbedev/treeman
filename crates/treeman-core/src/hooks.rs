//! Execute the `hooks.<phase>` steps from a loaded Config.
//!
//! Model: every entry under `postcreate` / `predelete` / `postdelete`
//! is a "group". Each group is spawned as a single detached driver
//! (via `setsid`) that internally chains its commands with `&&` so a
//! failure short-circuits the rest of the group. Groups never wait
//! for each other — they all kick off in parallel and `run_hooks`
//! returns as soon as the drivers are spawned.
//!
//! `precreate` is the only synchronous phase. It runs each
//! [`SingleStep`] in sequence and short-circuits on the first
//! non-zero exit, since by name it gates the worktree being usable.
//!
//! Subprocesses inherit `$PATH` (and every other env var) from
//! `inherited_env`, captured by the CLI at invocation time and
//! threaded through the daemon RPC. This is what lets a hook find
//! `composer` / `yarn` / `php` when the daemon itself is running
//! under systemd-user with a minimal env.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::process::Stdio;

use anyhow::{Context, Result};
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Command;

use crate::config::{HookEntry, SingleStep};

#[derive(Debug, Clone)]
pub struct HookRunOutcome {
    pub groups: Vec<GroupOutcome>,
    /// Always 0 for the async-group runner — drivers are detached and
    /// no caller is waiting. The aggregate is populated by the sync
    /// precreate path only.
    pub aggregate_exit_code: i32,
}

#[derive(Debug, Clone)]
pub struct GroupOutcome {
    /// Full driver command (the `cmd1 && cmd2` synthesis sent to sh).
    pub command: String,
    /// Detached-driver PID, if spawn succeeded.
    pub pid: Option<u32>,
    /// Path to the driver's combined stdout/stderr log inside the
    /// worktree (`.treeman-hooks/<phase>-<group-idx>.log`). The
    /// runner returns this so callers can surface a "tail -f X" hint.
    pub log_path: PathBuf,
    /// Sync-only field — non-zero when a foreground (precreate) step
    /// failed. Always 0 for async groups.
    pub exit_code: i32,
    pub stdout_tail: String,
    pub stderr_tail: String,
}

/// Spawn one detached driver per group, in parallel. Returns as soon
/// as all drivers have been forked. Driver logs land in
/// `<worktree>/.treeman-hooks/<phase>-<group-index>.log`.
pub async fn run_hooks(
    phase: &str,
    entries: &[HookEntry],
    repo_root: &Path,
    worktree_path: &Path,
    slug: &str,
    inherited_env: &BTreeMap<String, String>,
) -> Result<HookRunOutcome> {
    let log_dir = worktree_path.join(".treeman-hooks");
    if !entries.is_empty() {
        std::fs::create_dir_all(&log_dir).with_context(|| {
            format!("create hook log dir {}", log_dir.display())
        })?;
    }
    let mut outcomes = Vec::with_capacity(entries.len());
    for (idx, entry) in entries.iter().enumerate() {
        let sequence = entry.into_sequence();
        if sequence.is_empty() {
            continue;
        }
        let log_path = log_dir.join(format!("{phase}-{idx}.log"));
        let driver_cmd = render_group(&sequence, worktree_path);
        let pid = spawn_detached(
            &driver_cmd,
            worktree_path,
            repo_root,
            slug,
            &log_path,
            inherited_env,
        )?;
        outcomes.push(GroupOutcome {
            command: driver_cmd,
            pid: Some(pid),
            log_path,
            exit_code: 0,
            stdout_tail: String::new(),
            stderr_tail: String::new(),
        });
    }
    Ok(HookRunOutcome {
        groups: outcomes,
        aggregate_exit_code: 0,
    })
}

/// Synchronous precreate runner. Each step awaited in order; first
/// non-zero exit aborts the create. No detachment, no driver log
/// files — output captured as tails in [`GroupOutcome`].
pub async fn run_precreate_hooks(
    steps: &[SingleStep],
    repo_root: &Path,
    worktree_path: &Path,
    slug: &str,
    inherited_env: &BTreeMap<String, String>,
) -> Result<HookRunOutcome> {
    let mut outcomes = Vec::with_capacity(steps.len());
    let mut aggregate: i32 = 0;
    for step in steps {
        if aggregate != 0 {
            outcomes.push(GroupOutcome {
                command: step.run.clone(),
                pid: None,
                log_path: PathBuf::new(),
                exit_code: -1,
                stdout_tail: String::new(),
                stderr_tail: "skipped (prior step failed)".into(),
            });
            continue;
        }
        let cwd = step.cwd.as_deref().unwrap_or(worktree_path);
        let out = run_foreground(
            &step.run,
            cwd,
            repo_root,
            worktree_path,
            slug,
            inherited_env,
        )
        .await?;
        if out.exit_code != 0 && aggregate == 0 {
            aggregate = out.exit_code;
        }
        outcomes.push(out);
    }
    Ok(HookRunOutcome {
        groups: outcomes,
        aggregate_exit_code: aggregate,
    })
}

/// Render a sequence of `SingleStep`s into a single shell command
/// string the driver will run. Per-step `cwd` is rewritten as
/// `( cd <cwd> && <cmd> )` so it scopes correctly. Multiple steps
/// chained with `&&` so a failure short-circuits the rest.
fn render_group(sequence: &[SingleStep], default_cwd: &Path) -> String {
    let mut parts: Vec<String> = Vec::with_capacity(sequence.len());
    for step in sequence {
        let cwd = step.cwd.as_deref().unwrap_or(default_cwd);
        // Single-quote the cwd path so spaces / shell metas are inert.
        // The path is user-controlled config; if it contains `'` we
        // escape with the standard `'\''` sequence.
        let cwd_quoted = shell_single_quote(&cwd.to_string_lossy());
        // Step `run:` is intentionally NOT quoted — users write
        // arbitrary shell there ("yarn install --frozen-lockfile",
        // pipelines, etc.). The runner just dispatches.
        parts.push(format!("( cd {cwd_quoted} && {} )", step.run));
    }
    parts.join(" && ")
}

fn shell_single_quote(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('\'');
    for c in s.chars() {
        if c == '\'' {
            out.push_str("'\\''");
        } else {
            out.push(c);
        }
    }
    out.push('\'');
    out
}

fn spawn_detached(
    cmd: &str,
    worktree_path: &Path,
    repo_root: &Path,
    slug: &str,
    log_path: &Path,
    inherited_env: &BTreeMap<String, String>,
) -> Result<u32> {
    // Open the log file (truncate) BEFORE spawning so we can hand the
    // descriptor to the child's stdout+stderr. Driver writes are
    // combined so users get a single `tail -f`-able file per group.
    let log_file = std::fs::OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(true)
        .open(log_path)
        .with_context(|| format!("open hook log {}", log_path.display()))?;
    let log_for_stderr = log_file.try_clone().with_context(|| {
        format!("clone hook log fd {}", log_path.display())
    })?;

    let mut spawn = std::process::Command::new("setsid");
    spawn
        .arg("/bin/sh")
        .arg("-c")
        .arg(cmd)
        .current_dir(worktree_path)
        .stdin(Stdio::null())
        .stdout(Stdio::from(log_file))
        .stderr(Stdio::from(log_for_stderr));

    // env_clear() drops the daemon's systemd-user env entirely; we
    // then layer the caller's interactive-shell env + the three
    // standard overlays. This is what makes `composer` / `yarn` /
    // `php` resolvable: the daemon's $PATH would otherwise be just
    // /usr/bin:/bin.
    spawn.env_clear();
    for (k, v) in inherited_env {
        spawn.env(k, v);
    }
    spawn.env("GWT_MAIN", repo_root);
    spawn.env("GWT_WT", worktree_path);
    spawn.env("TREEMAN_SLUG", slug);

    let child = spawn.spawn().with_context(|| format!("setsid spawn `{cmd}`"))?;
    Ok(child.id())
}

async fn run_foreground(
    cmd: &str,
    cwd: &Path,
    repo_root: &Path,
    worktree_path: &Path,
    slug: &str,
    inherited_env: &BTreeMap<String, String>,
) -> Result<GroupOutcome> {
    let mut spawn = Command::new("/bin/sh");
    spawn
        .arg("-c")
        .arg(cmd)
        .current_dir(cwd)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    spawn.env_clear();
    for (k, v) in inherited_env {
        spawn.env(k, v);
    }
    spawn.env("GWT_MAIN", repo_root);
    spawn.env("GWT_WT", worktree_path);
    spawn.env("TREEMAN_SLUG", slug);

    let mut child = spawn.spawn().with_context(|| format!("spawn `{cmd}`"))?;

    // Capture tails (last 16 KiB) so we don't blow up the events table.
    let stdout = child.stdout.take().expect("piped");
    let stderr = child.stderr.take().expect("piped");
    let stdout_task = tokio::spawn(capture_tail(stdout, 16 * 1024));
    let stderr_task = tokio::spawn(capture_tail(stderr, 16 * 1024));

    let status = child.wait().await.context("wait child")?;
    let stdout_tail = stdout_task.await.unwrap_or_default();
    let stderr_tail = stderr_task.await.unwrap_or_default();

    Ok(GroupOutcome {
        command: cmd.to_string(),
        pid: None,
        log_path: PathBuf::new(),
        exit_code: status.code().unwrap_or(-1),
        stdout_tail,
        stderr_tail,
    })
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
        let n = match reader.read_line(&mut line).await {
            Ok(n) => n,
            Err(_) => break,
        };
        if n == 0 {
            break;
        }
        buf.extend_from_slice(line.as_bytes());
        if buf.len() > cap_bytes {
            let drop_n = buf.len() - cap_bytes;
            buf.drain(0..drop_n);
        }
    }
    String::from_utf8_lossy(&buf).to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Instant;

    fn empty_env() -> BTreeMap<String, String> {
        let mut e = BTreeMap::new();
        // /bin and /usr/bin so `sh`, `touch`, `sleep`, `echo`, `cat`
        // are all on $PATH — these tests run hermetically without
        // the host's nix-profile etc.
        e.insert("PATH".into(), "/usr/bin:/bin".into());
        e
    }

    fn tmpdir(label: &str) -> PathBuf {
        let p = std::env::temp_dir().join(format!(
            "treeman-hooks-{label}-{}",
            blake3::hash(format!("{:?}", Instant::now()).as_bytes()).to_hex()
        ));
        std::fs::create_dir_all(&p).unwrap();
        p
    }

    #[tokio::test]
    async fn group_runs_sequence_within() {
        let wt = tmpdir("seq");
        let entries = vec![HookEntry::Group(vec![
            crate::config::GroupChild::Cmd("touch a".into()),
            crate::config::GroupChild::Cmd("touch b".into()),
        ])];
        let out =
            run_hooks("postcreate", &entries, &wt, &wt, "slug", &empty_env())
                .await
                .unwrap();
        // Wait for the detached driver to finish.
        wait_until(|| wt.join("b").exists(), 2000).await;
        // Both files exist + b created after a.
        let a_t = std::fs::metadata(wt.join("a")).unwrap().modified().unwrap();
        let b_t = std::fs::metadata(wt.join("b")).unwrap().modified().unwrap();
        assert!(b_t >= a_t, "b mtime ({b_t:?}) must be ≥ a mtime ({a_t:?})");
        assert_eq!(out.groups.len(), 1);
        std::fs::remove_dir_all(&wt).ok();
    }

    #[tokio::test]
    async fn groups_run_parallel_across() {
        let wt = tmpdir("par");
        let started = Instant::now();
        let entries = vec![
            HookEntry::Cmd("sleep 0.5 && touch one".into()),
            HookEntry::Cmd("sleep 0.5 && touch two".into()),
        ];
        let _ =
            run_hooks("postcreate", &entries, &wt, &wt, "slug", &empty_env())
                .await
                .unwrap();
        // run_hooks itself must return immediately — driver spawn only.
        assert!(
            started.elapsed().as_millis() < 200,
            "run_hooks returned in {}ms — should be <200ms",
            started.elapsed().as_millis()
        );
        // Both files appear within ~0.6s if groups ran in parallel,
        // not 1.0s+.
        wait_until(|| wt.join("one").exists() && wt.join("two").exists(), 1200)
            .await;
        let total = started.elapsed().as_millis();
        assert!(total < 900, "parallel groups took {total}ms — should be <900ms");
        std::fs::remove_dir_all(&wt).ok();
    }

    #[tokio::test]
    async fn run_hooks_returns_fast_regardless_of_command_duration() {
        let wt = tmpdir("fast");
        let entries = vec![HookEntry::Cmd("sleep 10".into())];
        let started = Instant::now();
        run_hooks("postcreate", &entries, &wt, &wt, "slug", &empty_env())
            .await
            .unwrap();
        let elapsed = started.elapsed().as_millis();
        assert!(elapsed < 200, "run_hooks blocked for {elapsed}ms");
        std::fs::remove_dir_all(&wt).ok();
    }

    #[tokio::test]
    async fn inherited_env_reaches_subprocess() {
        let wt = tmpdir("env");
        let mut env = empty_env();
        env.insert("TREEMAN_TEST_PATH_PROBE".into(), "xyz123".into());
        let entries = vec![HookEntry::Cmd(
            "printenv TREEMAN_TEST_PATH_PROBE > probe.out".into(),
        )];
        run_hooks("postcreate", &entries, &wt, &wt, "slug", &env)
            .await
            .unwrap();
        wait_until(|| wt.join("probe.out").exists(), 1500).await;
        let v = std::fs::read_to_string(wt.join("probe.out")).unwrap();
        assert_eq!(v.trim(), "xyz123");
        std::fs::remove_dir_all(&wt).ok();
    }

    #[tokio::test]
    async fn precreate_short_circuits_on_failure() {
        let wt = tmpdir("pre");
        let steps = vec![
            SingleStep {
                run: "touch ran-first".into(),
                cwd: None,
            },
            SingleStep {
                run: "false".into(),
                cwd: None,
            },
            SingleStep {
                run: "touch ran-third".into(),
                cwd: None,
            },
        ];
        let out =
            run_precreate_hooks(&steps, &wt, &wt, "slug", &empty_env())
                .await
                .unwrap();
        assert_eq!(out.aggregate_exit_code, 1);
        assert!(wt.join("ran-first").exists());
        assert!(!wt.join("ran-third").exists(), "third step must be skipped");
        std::fs::remove_dir_all(&wt).ok();
    }

    #[test]
    fn render_group_chains_with_and() {
        let wt = std::path::Path::new("/tmp/x");
        let seq = vec![
            SingleStep {
                run: "echo a".into(),
                cwd: None,
            },
            SingleStep {
                run: "echo b".into(),
                cwd: Some(PathBuf::from("/tmp/sub")),
            },
        ];
        let rendered = render_group(&seq, wt);
        assert!(rendered.contains(" && "));
        assert!(rendered.contains("cd '/tmp/x' && echo a"));
        assert!(rendered.contains("cd '/tmp/sub' && echo b"));
    }

    #[test]
    fn shell_single_quote_escapes_apostrophes() {
        assert_eq!(shell_single_quote("plain"), "'plain'");
        assert_eq!(shell_single_quote("a'b"), "'a'\\''b'");
    }

    async fn wait_until<F: Fn() -> bool>(cond: F, max_ms: u64) {
        let start = Instant::now();
        while start.elapsed().as_millis() < max_ms as u128 {
            if cond() {
                return;
            }
            tokio::time::sleep(std::time::Duration::from_millis(25)).await;
        }
    }
}
