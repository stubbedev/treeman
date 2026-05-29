// Package daemonctl starts and stops `treemand` without shelling
// through `treeman daemon …`. Shared by the CLI (`treeman daemon
// start|stop`) and the MCP `daemon_control` tool so neither needs to
// fork the parent binary just to flip the daemon's state.
package daemonctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/stubbedev/treeman/internal/rpc"
)

// LaunchdLabel is the LaunchAgent identifier the macOS installer
// uses. Exposed so callers can mirror it from a single source.
const LaunchdLabel = "dev.stubbe.treemand"

// goos is the OS selector for Start/Stop. It mirrors runtime.GOOS in
// production and is overridable in tests so the launchd (darwin) and
// systemd (linux) branches can both be exercised on a single host.
var goos = runtime.GOOS

// Start brings treemand up. Tries the OS-native init first (systemd
// --user on Linux, launchctl kickstart on macOS) so an installed
// auto-start unit keeps managing the process; otherwise it forks
// the `treemand` binary detached and returns the spawned PID.
//
// Returns (pid, nil) when the binary was forked; (0, nil) when the
// init system handled it.
func Start(ctx context.Context) (int, error) {
	switch goos {
	case "darwin":
		uid := os.Getuid()
		domain := fmt.Sprintf("gui/%d", uid)
		if err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", domain+"/"+LaunchdLabel).Run(); err == nil {
			return 0, nil
		}
	default:
		if err := exec.CommandContext(ctx, "systemctl", "--user", "is-enabled", "treemand").Run(); err == nil {
			return 0, exec.CommandContext(ctx, "systemctl", "--user", "start", "treemand").Run()
		}
	}

	binPath, err := exec.LookPath("treemand")
	if err != nil {
		return 0, fmt.Errorf("treemand not on PATH: %w", err)
	}
	cmd := exec.Command(binPath) //nolint:noctx // detached daemon fork; must outlive caller ctx
	cmd.Stdin = nil
	cmd.Stdout, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// Stop asks treemand to shut down. Prefers the OS-native init when
// available so an auto-start unit doesn't immediately respawn the
// process; falls back to the shutdown RPC.
func Stop(ctx context.Context) error {
	switch goos {
	case "darwin":
		uid := os.Getuid()
		domain := fmt.Sprintf("gui/%d", uid)
		if err := exec.CommandContext(ctx, "launchctl", "kill", "TERM", domain+"/"+LaunchdLabel).Run(); err == nil {
			return nil
		}
	default:
		if err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "treemand").Run(); err == nil {
			return exec.CommandContext(ctx, "systemctl", "--user", "stop", "treemand").Run()
		}
	}
	resp, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodShutdown})
	if err != nil {
		return err
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	return nil
}

// StopViaRPC bypasses the OS init layer and asks the daemon directly
// via its shutdown RPC. Used by callers that don't want to disturb
// any installed auto-start unit (e.g. MCP agents that only have
// daemon-socket access).
func StopViaRPC(ctx context.Context) error {
	resp, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodShutdown})
	if err != nil {
		return err
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	return nil
}
