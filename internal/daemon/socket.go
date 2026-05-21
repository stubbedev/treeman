// Package daemon hosts the long-running treemand process: socket
// listener, per-repo watcher fanout, RPC dispatch.
package daemon

import (
	"os"
)

// Lockdown chmod 0600 on the socket so other users can't connect.
// The 0600 mode plus SO_PEERCRED (Linux) / Xucred (Darwin) on
// CheckPeerUID gives defence in depth.
func Lockdown(path string) error {
	return os.Chmod(path, 0o600)
}

// RemoveStale unlinks an existing socket file before bind. Also
// drops the legacy `<sock>.addr` sidecar from the very-old M0 era.
func RemoveStale(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + ".addr")
}
