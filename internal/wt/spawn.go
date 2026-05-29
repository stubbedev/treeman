package wt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
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

// DetachFinalize spawns `treeman wt finalize --local <wtPath>` in a
// new session via setsid so it survives the parent's exit.
// stdout + stderr stream to <wtPath>/.treeman-hooks/fg-finalize.log.
// Returns the log path for the success message.
func DetachFinalize(wtPath, repoRoot string) (string, error) {
	return detachChild(
		filepath.Join(wtPath, ".treeman-hooks", "fg-finalize.log"),
		"wt", "finalize", "--local", "--repo", repoRoot, wtPath,
	)
}

// DetachDelete spawns `treeman wt delete --detached <wtPath>` (with
// --force / --yes when needed) in a fresh session.
func DetachDelete(wtPath, repoRoot string, force bool) (string, error) {
	args := []string{"wt", "delete", "--detached", "--yes", "--repo", repoRoot}
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
// the caller can surface it to the user.
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
	cmd := exec.Command(bin, args...) //nolint:noctx // detached setsid child; must outlive caller ctx
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "", err
	}
	_ = logFile.Close()
	_ = cmd.Process.Release()
	return logPath, nil
}
