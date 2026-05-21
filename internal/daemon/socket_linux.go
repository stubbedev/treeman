//go:build linux

package daemon

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// CheckPeerUID enforces SO_PEERCRED on every accepted connection so
// only the user that owns the daemon socket can connect.
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
