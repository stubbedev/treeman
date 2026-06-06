//go:build e2e

// Package loglevel_e2e verifies daemon.log_level actually controls the
// daemon's stderr verbosity end-to-end: the startup loop messages are
// emitted at INFO, so log_level=error must suppress them while the
// default (info) shows them. No engines needed — treemand logs these at
// boot.
package loglevel_e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func buildTreemand(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(filepath.Dir(root))
	dir, err := os.MkdirTemp("", "treemand-loglevel-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "treemand")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/treemand")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build treemand: %v", err)
	}
	return bin
}

// daemonStderr boots treemand with the given global log_level, lets it
// emit its startup logs, kills it, and returns captured stderr.
func daemonStderr(t *testing.T, bin, logLevel string) string {
	t.Helper()
	xdgConfig := t.TempDir()
	if logLevel != "" {
		cfgDir := filepath.Join(xdgConfig, "treeman")
		if err := os.MkdirAll(cfgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"),
			[]byte("daemon:\n  log_level: "+logLevel+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+xdgConfig,
		"XDG_STATE_HOME="+t.TempDir(),
		"XDG_DATA_HOME="+t.TempDir(),
		"XDG_RUNTIME_DIR="+t.TempDir(),
		"TREEMAN_DB_PATH="+filepath.Join(t.TempDir(), "treeman.db"),
		"TREEMAN_SOCKET="+filepath.Join(t.TempDir(), "tm.sock"),
	)
	var serr strings.Builder
	cmd.Stderr = &serr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start treemand: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(1500 * time.Millisecond) // let the startup loops log
	return serr.String()
}

func TestLogLevelControlsVerbosity(t *testing.T) {
	bin := buildTreemand(t)

	// Default (info): startup loop messages are emitted at INFO.
	infoOut := daemonStderr(t, bin, "info")
	if !strings.Contains(infoOut, "level=INFO") {
		t.Fatalf("log_level=info: expected INFO startup logs, got:\n%s", infoOut)
	}

	// error: the same INFO startup messages must be suppressed.
	errOut := daemonStderr(t, bin, "error")
	if strings.Contains(errOut, "level=INFO") {
		t.Errorf("log_level=error did not suppress INFO logs:\n%s", errOut)
	}
}
