//! Wire types shared between `treemand` (daemon) and `treeman` (CLI).
//!
//! Kept dependency-light so the CLI client links without dragging in the
//! daemon's runtime, db drivers, or sqlite.

use serde::{Deserialize, Serialize};

pub const PROTOCOL_VERSION: u32 = 1;

/// Default socket-path lookup order: `$TREEMAN_SOCKET`, then
/// `$XDG_RUNTIME_DIR/treeman.sock`, then `~/.local/state/treeman/treeman.sock`.
pub const SOCKET_ENV: &str = "TREEMAN_SOCKET";
pub const SOCKET_BASENAME: &str = "treeman.sock";

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct StatusResponse {
    pub protocol_version: u32,
    pub daemon_version: String,
    pub pid: u32,
    pub started_at_unix: i64,
    pub watcher_count: u32,
}

/// CLI → daemon. One per request; daemon writes one `Response` and
/// either closes the connection or keeps it open for the next line.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "method", rename_all = "snake_case")]
pub enum Request {
    Status,
    Ping,
    /// Register or update a repo (path + name).
    RepoRegister {
        path: String,
        name: String,
    },
    /// Start a per-repo watcher in the daemon.
    WatcherStart {
        repo_path: String,
    },
    /// Stop a previously-started watcher.
    WatcherStop {
        repo_path: String,
    },
    /// List currently-running watchers.
    WatcherList,
    /// List linked worktrees currently being watched for a given repo.
    WorktreeList {
        repo_path: String,
    },
    /// Run the postcreate hooks + prepare for a worktree in the
    /// daemon's runtime so the caller (e.g. `treeman wt create`)
    /// can return immediately. The daemon detaches a tokio task,
    /// streams output into the SQLite event log, and survives the
    /// caller's shell exiting. See `worktrees.async_create` in
    /// `.treeman.yaml` — set it to `true` to make `treeman wt create`
    /// fan this off automatically.
    WorktreeFinalize {
        repo_path: String,
        worktree_path: String,
    },
    /// Tell the daemon to shut down cleanly (used by `treeman daemon stop`).
    Shutdown,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Response {
    Ok,
    Pong,
    Status(StatusResponse),
    RepoRegistered {
        repo_id: i64,
    },
    WatcherStarted {
        repo_path: String,
    },
    WatcherStopped {
        repo_path: String,
    },
    WatcherList {
        repos: Vec<WatcherSummary>,
    },
    WorktreeList {
        worktrees: Vec<String>,
    },
    /// Daemon accepted the finalize request and detached a task.
    /// The caller should not wait — use `treeman logs tail -f` to
    /// follow progress.
    WorktreeFinalizeQueued {
        worktree_path: String,
    },
    Error {
        message: String,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct WatcherSummary {
    pub repo: String,
    pub worktree_count: u32,
}

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum HookPhase {
    Precreate,
    Postcreate,
    Predelete,
    Postdelete,
}

impl HookPhase {
    pub fn as_str(self) -> &'static str {
        match self {
            HookPhase::Precreate => "precreate",
            HookPhase::Postcreate => "postcreate",
            HookPhase::Predelete => "predelete",
            HookPhase::Postdelete => "postdelete",
        }
    }
}

impl std::str::FromStr for HookPhase {
    type Err = String;
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s {
            "precreate" => Ok(HookPhase::Precreate),
            "postcreate" => Ok(HookPhase::Postcreate),
            "predelete" => Ok(HookPhase::Predelete),
            "postdelete" => Ok(HookPhase::Postdelete),
            other => Err(format!("unknown hook phase: {other}")),
        }
    }
}
