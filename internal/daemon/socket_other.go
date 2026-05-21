//go:build !linux

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
)

// CheckPeerUID — non-Linux fallback. SO_PEERCRED is a Linux-only
// socket option; macOS/BSD use LOCAL_PEERCRED + Xucred, which Go's
// stdlib doesn't expose without pulling golang.org/x/sys.
//
// Instead we rely on the socket's filesystem mode (Lockdown sets
// 0600) and verify the socket file's owning UID matches the daemon's
// effective UID. This isn't as airtight as SO_PEERCRED — a
// determined attacker who can already write into XDG_RUNTIME_DIR
// could spoof — but darwin/BSD dev workstations are single-user
// targets where the filesystem-mode + owner check is the
// pragmatically-correct trust boundary.
func CheckPeerUID(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("peer uid check: not a unix conn (%T)", c)
	}
	// Resolve the socket path from the local address so we can stat
	// it. On Darwin, the unix socket path is the only piece of state
	// we can trust.
	addr := uc.LocalAddr()
	if addr == nil {
		return fmt.Errorf("peer uid check: nil LocalAddr")
	}
	path := addr.String()
	fi, err := os.Lstat(path)
	if err != nil {
		// TOCTOU hardening: if the socket file vanished between bind
		// and now (ENOENT), an attacker may have unlinked it and
		// rebound under another uid. Refuse — there's no way left to
		// verify the owner. Any other stat error is also a refuse.
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("socket %s missing — refusing connection", path)
		}
		return fmt.Errorf("stat socket %s: %w", path, err)
	}
	// Reject anything that isn't a unix-domain socket file (someone
	// swapped a regular file or symlink into place).
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket %s: not a socket (mode %s)", path, fi.Mode())
	}
	// Permissions must remain 0600 — Lockdown set them; if they've
	// loosened, something else touched the file.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		return fmt.Errorf("socket %s: unexpected perms %o (want 0600)", path, perm)
	}
	sysStat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat socket %s: no syscall.Stat_t", path)
	}
	our := uint32(os.Geteuid())
	if sysStat.Uid != our {
		return fmt.Errorf("socket owner uid %d != daemon uid %d", sysStat.Uid, our)
	}
	return nil
}
