//go:build !linux

package daemon

import (
	"fmt"
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
// could spoof — but the kontainer + dev-workstation deploy targets
// are single-user, and the daemon is only built for darwin so the
// developer's laptop can use treeman locally outside of the prod
// Linux env.
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
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat socket %s: %w", path, err)
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
