// Background-execution helpers for the foreground fallback paths
// of `wt create` and `wt delete`. When the daemon RPC is unreachable
// and we can't get the daemon to come back up, the alternative used
// to be running the heavy tail synchronously — minutes of blocking
// for composer install, dump load, migrate, snapshot, paratest fan-
// out. The helpers below fork a detached child treeman process so
// the interactive shell never has to wait on that work.
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stubbedev/treeman/internal/rpc"
)

// ensureDaemon tries to reach the daemon. If the ping fails it
// invokes `treeman daemon start` semantics inline (systemd / launchd
// when installed, else a detached binary), then polls the socket
// for up to ~2s. Returns nil when the daemon is reachable; an error
// otherwise.
//
// Used at the top of every fallback path so a freshly-rebooted host
// (or a daemon that crashed once and didn't get auto-restarted)
// doesn't force a foreground-blocking degraded mode just because the
// first RPC attempt missed.
func ensureDaemon(ctx context.Context) error {
	if _, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodPing}); err == nil {
		return nil
	}
	if _, err := startDaemonProcess(ctx); err != nil {
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
	return fmt.Errorf("daemon did not respond within 2s")
}

// detachLocalFinalize spawns `treeman wt finalize --local <wtPath>`
// in a new session via setsid so it survives the parent's exit.
// stdout + stderr stream to <wtPath>/.treeman-hooks/fg-finalize.log
// so the user can tail-follow progress without an attached terminal.
// Returns the log path for the success message.
func detachLocalFinalize(wtPath, repoRoot string) (string, error) {
	return detachChild(
		filepath.Join(wtPath, ".treeman-hooks", "fg-finalize.log"),
		"wt", "finalize", "--local", "--repo", repoRoot, wtPath,
	)
}

// detachLocalDelete spawns `treeman wt delete --foreground <wtPath>`
// (with --force when requested) under a new session so the slow
// predelete + db-teardown + git-remove sequence runs without
// blocking the calling shell.
func detachLocalDelete(wtPath, repoRoot string, force bool) (string, error) {
	args := []string{"wt", "delete", "--foreground", "--repo", repoRoot}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wtPath)
	return detachChild(
		filepath.Join(repoRoot, ".treeman-hooks", "fg-delete-"+filepath.Base(wtPath)+".log"),
		args...,
	)
}

// detachChild runs `treeman <args>` in a fresh session. Stdin is
// detached, stdout+stderr point at logPath. Returns the log path so
// the caller can surface it to the user. The child outlives the
// parent process via Setsid + Process.Release().
func detachChild(logPath string, args ...string) (string, error) {
	bin, err := os.Executable()
	if err != nil || bin == "" {
		bin, err = exec.LookPath("treeman")
		if err != nil {
			return "", fmt.Errorf("locate treeman binary: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "", err
	}
	// Close our handle to the log file — the child holds its own
	// dup'd descriptors. Release the os.Process so we don't sit on a
	// zombie when the child finishes.
	_ = logFile.Close()
	_ = cmd.Process.Release()
	return logPath, nil
}
