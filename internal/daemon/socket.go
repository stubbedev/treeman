// Package daemon hosts the long-running treemand process: socket
// listener, per-repo watcher fanout, RPC dispatch. Ported from
// `crates/treeman-daemon/src/`.
package daemon

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// CheckPeerUID enforces SO_PEERCRED on every accepted connection so
// only the user that owns the daemon socket can connect. Matches the
// behaviour of `crates/treeman-daemon/src/socket.rs`.
func CheckPeerUID(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("peer uid check: not a unix conn (%T)", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var ucred *syscall.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		ucred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if sockErr != nil {
		return sockErr
	}
	our := uint32(os.Geteuid())
	if ucred.Uid != our {
		return fmt.Errorf("peer uid %d != daemon uid %d", ucred.Uid, our)
	}
	return nil
}

// Lockdown chmod 0600 on the socket so other users can't connect.
func Lockdown(path string) error {
	return os.Chmod(path, 0o600)
}

// RemoveStale unlinks an existing socket file before bind. Also
// drops the legacy `<sock>.addr` sidecar from the very-old M0 era.
func RemoveStale(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + ".addr")
}
