package wt

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/stubbedev/treeman/internal/daemonctl"
	"github.com/stubbedev/treeman/internal/rpc"
)

// EnsureDaemon tries to reach the daemon. If the ping fails it
// invokes `daemonctl.Start` inline (systemd / launchd when
// installed, else a detached binary), then polls the socket for up
// to ~2s. Returns nil when the daemon is reachable; an error
// otherwise.
func EnsureDaemon(ctx context.Context) error {
	if _, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodPing}); err == nil {
		return nil
	}
	if _, err := daemonctl.Start(ctx); err != nil {
		return fmt.Errorf("daemon start: %w", err)
	}
	sock, _ := rpc.SocketPath()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			if _, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodPing}); err == nil {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not respond within 2s — %s", daemonDebugHint())
}

// daemonDebugHint returns a one-liner pointing at the right log
// surface for the current platform.
func daemonDebugHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "check `log show --predicate 'process == \"treemand\"' --last 5m` and run `treeman doctor`"
	default:
		return "check `journalctl --user -u treemand -n 50` and run `treeman doctor`"
	}
}

// Note: teardown/finalize are dispatched to the daemon over the RPC
// socket (see dispatch.go + CallWithStart). There is deliberately no
// "detach a CLI child to do the work" fallback — when the daemon can't
// be reached even after an autostart attempt, create/delete surface a
// hard error instead of forking a `treeman worktree …` subprocess.
