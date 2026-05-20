//! Slug derivation. Ports the bash `slug_for()` from `.gwt-postcreate` /
//! `.gwt-predelete` so a worktree that was named by `gwt` keeps the same
//! identifier when treeman owns the lifecycle.
//!
//! Rules:
//!   - If a branch name (or, as fallback, the last path component) contains
//!     `[A-Z]+-\d+` (a Jira ticket key), the slug is `<prefix>_<number>`
//!     lowercased.
//!   - Otherwise, slug = `wt_<blake3(canonical-path)[..8]>` — deterministic
//!     and distinct between two worktrees whose basenames collide.
//!
//! Derived properties (`slug_dash`, `slug_redis_queue`, `slug_redis_cache`)
//! match the bash impl exactly — see `derive_redis_indices` for the cksum
//! quirk: bash `cksum` is the SysV checksum, which we re-implement here so
//! a worktree that already exists doesn't change its redis db on the switch
//! to treeman.

use std::path::Path;

use regex::Regex;
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Slug {
    pub value: String,
    pub source: SlugSource,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum SlugSource {
    Ticket,
    PathHash,
}

impl Slug {
    pub fn as_str(&self) -> &str {
        &self.value
    }
    pub fn dashed(&self) -> String {
        self.value.replace('_', "-")
    }
    /// (queue_db, cache_db) — bash `cksum` parity. See module-level docs.
    pub fn redis_indices(&self) -> (u8, u8) {
        let h = sysv_cksum(self.value.as_bytes());
        let queue = (h % 10 + 6) as u8;
        let cache = ((h / 10) % 10 + 6) as u8;
        (queue, cache)
    }
}

/// `branch` is consulted first (matches the bash flow: a ticket-named
/// branch wins even if the worktree dir was named generically). Then the
/// worktree's last path component is checked. Finally we hash the
/// canonical path.
pub fn slug_for(worktree_path: &Path, branch: Option<&str>) -> Slug {
    let ticket_re = ticket_regex();
    if let Some(b) = branch {
        if let Some(s) = extract_ticket(b, &ticket_re) {
            return Slug {
                value: s,
                source: SlugSource::Ticket,
            };
        }
    }
    if let Some(last) = worktree_path.file_name().and_then(|s| s.to_str()) {
        if let Some(s) = extract_ticket(last, &ticket_re) {
            return Slug {
                value: s,
                source: SlugSource::Ticket,
            };
        }
    }
    let canonical = worktree_path
        .canonicalize()
        .unwrap_or_else(|_| worktree_path.to_path_buf());
    let bytes = canonical.as_os_str().as_encoded_bytes();
    let h = blake3::hash(bytes);
    let hex = h.to_hex();
    Slug {
        value: format!("wt_{}", &hex[..8]),
        source: SlugSource::PathHash,
    }
}

fn extract_ticket(s: &str, re: &Regex) -> Option<String> {
    re.captures(s).map(|caps| {
        let prefix = caps.get(1).unwrap().as_str().to_lowercase();
        let num = caps.get(2).unwrap().as_str();
        let mut out = format!("{prefix}_{num}");
        if out.len() > 32 {
            out.truncate(32);
        }
        out
    })
}

fn ticket_regex() -> Regex {
    // [A-Z]+-[0-9]+
    Regex::new(r"([A-Z]+)-([0-9]+)").expect("static regex")
}

/// SysV `cksum` (POSIX) — matches `printf '%s' SLUG | cksum | awk '{print $1}'`
/// used by the bash hook. We must reproduce the exact algorithm so a
/// worktree's redis db indices don't shift on the switch from bash → treeman.
fn sysv_cksum(input: &[u8]) -> u32 {
    // POSIX cksum: CRC-32 over (data || length-bytes-little-endian-by-octets).
    // Algorithm per `man cksum`. Initial CRC=0, no inversion, polynomial
    // 0x04C11DB7 with the high bit out. We compute it byte-by-byte; the
    // length suffix encodes each octet of the length, LSB first, ending
    // when the remaining length is zero.
    let mut crc: u32 = 0;
    for &b in input {
        crc = crc_update(crc, b);
    }
    let mut len = input.len() as u64;
    while len != 0 {
        crc = crc_update(crc, (len & 0xff) as u8);
        len >>= 8;
    }
    !crc
}

fn crc_update(mut crc: u32, byte: u8) -> u32 {
    crc ^= (byte as u32) << 24;
    for _ in 0..8 {
        if crc & 0x8000_0000 != 0 {
            crc = (crc << 1) ^ 0x04C1_1DB7;
        } else {
            crc <<= 1;
        }
    }
    crc
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    #[test]
    fn ticket_in_branch_wins() {
        let s = slug_for(
            &PathBuf::from("/tmp/random-dir"),
            Some("feature/KON-1234-foo"),
        );
        assert_eq!(s.value, "kon_1234");
        assert_eq!(s.source, SlugSource::Ticket);
    }

    #[test]
    fn ticket_in_path_basename() {
        let s = slug_for(&PathBuf::from("/tmp/KON-9001-bar"), None);
        assert_eq!(s.value, "kon_9001");
    }

    #[test]
    fn falls_back_to_path_hash() {
        let s = slug_for(&PathBuf::from("/nonexistent/random"), None);
        assert!(s.value.starts_with("wt_"));
        assert_eq!(s.value.len(), 11);
        assert_eq!(s.source, SlugSource::PathHash);
    }

    #[test]
    fn slug_dashed_replaces_underscores() {
        let s = Slug {
            value: "kon_1234".into(),
            source: SlugSource::Ticket,
        };
        assert_eq!(s.dashed(), "kon-1234");
    }

    #[test]
    fn redis_indices_in_range_6_15() {
        let s = Slug {
            value: "kon_1234".into(),
            source: SlugSource::Ticket,
        };
        let (q, c) = s.redis_indices();
        assert!((6..=15).contains(&q));
        assert!((6..=15).contains(&c));
    }

    #[test]
    fn sysv_cksum_known_vectors() {
        // `printf '' | cksum`              => 4294967295 (0xFFFFFFFF), 0
        // `printf 'a' | cksum`             => 1220704766, 1
        // `printf 'abc' | cksum`           => 1219131554, 3
        // `printf 'kon_1234' | cksum`      => verified via `printf 'kon_1234' | cksum` on linux
        assert_eq!(sysv_cksum(b""), 4294967295);
        assert_eq!(sysv_cksum(b"a"), 1220704766);
        assert_eq!(sysv_cksum(b"abc"), 1219131554);
    }
}
