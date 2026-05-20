use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use directories::ProjectDirs;
use tokio::net::UnixStream;
use treeman_proto::{SOCKET_BASENAME, SOCKET_ENV};

pub fn resolve_path() -> Result<PathBuf> {
    if let Ok(s) = std::env::var(SOCKET_ENV) {
        return Ok(PathBuf::from(s));
    }
    if let Ok(rt) = std::env::var("XDG_RUNTIME_DIR") {
        if !rt.is_empty() {
            return Ok(PathBuf::from(rt).join(SOCKET_BASENAME));
        }
    }
    let dirs = ProjectDirs::from("", "", "treeman").context("project dirs unavailable")?;
    let state = dirs.data_local_dir();
    std::fs::create_dir_all(state).with_context(|| format!("create {}", state.display()))?;
    Ok(state.join(SOCKET_BASENAME))
}

pub fn clear_stale(socket_path: &Path) -> Result<()> {
    let _ = std::fs::remove_file(socket_path);
    let _ = std::fs::remove_file(socket_path.with_extension("addr")); // legacy
    Ok(())
}

/// chmod 0600 on the socket so other users can't connect.
pub fn lockdown(socket_path: &Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    let perm = std::fs::Permissions::from_mode(0o600);
    std::fs::set_permissions(socket_path, perm)
        .with_context(|| format!("chmod 0600 {}", socket_path.display()))?;
    Ok(())
}

/// Peer-uid check. Rejects any connection where the peer uid !=
/// the daemon's uid. Uses `SO_PEERCRED` on Linux and `getpeereid(3)`
/// on macOS / *BSD.
pub fn check_peer_uid(stream: &UnixStream) -> Result<()> {
    let our_uid = unsafe { libc::geteuid() };
    let fd = stream.as_raw_fd();
    let peer = peer_uid(fd)?;
    if peer != our_uid {
        bail!("peer uid {} != daemon uid {}", peer, our_uid);
    }
    Ok(())
}

#[cfg(any(target_os = "linux", target_os = "android"))]
fn peer_uid(fd: std::os::unix::io::RawFd) -> Result<libc::uid_t> {
    #[repr(C)]
    struct Ucred {
        pid: libc::pid_t,
        uid: libc::uid_t,
        gid: libc::gid_t,
    }
    let mut cred = Ucred {
        pid: 0,
        uid: 0,
        gid: 0,
    };
    let mut len = std::mem::size_of::<Ucred>() as libc::socklen_t;
    let rc = unsafe {
        libc::getsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            &mut cred as *mut _ as *mut libc::c_void,
            &mut len,
        )
    };
    if rc != 0 {
        bail!(
            "getsockopt SO_PEERCRED: {}",
            std::io::Error::last_os_error()
        );
    }
    Ok(cred.uid)
}

#[cfg(any(
    target_os = "macos",
    target_os = "ios",
    target_os = "freebsd",
    target_os = "netbsd",
    target_os = "openbsd",
    target_os = "dragonfly",
))]
fn peer_uid(fd: std::os::unix::io::RawFd) -> Result<libc::uid_t> {
    let mut euid: libc::uid_t = 0;
    let mut egid: libc::gid_t = 0;
    let rc = unsafe { libc::getpeereid(fd, &mut euid, &mut egid) };
    if rc != 0 {
        bail!("getpeereid: {}", std::io::Error::last_os_error());
    }
    Ok(euid)
}

#[cfg(not(any(
    target_os = "linux",
    target_os = "android",
    target_os = "macos",
    target_os = "ios",
    target_os = "freebsd",
    target_os = "netbsd",
    target_os = "openbsd",
    target_os = "dragonfly",
)))]
fn peer_uid(_fd: std::os::unix::io::RawFd) -> Result<libc::uid_t> {
    bail!("peer-uid lookup not implemented on this OS")
}
