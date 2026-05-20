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
