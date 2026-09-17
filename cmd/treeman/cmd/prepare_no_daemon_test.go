package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/ui"
)

func TestPrepareNoDaemon(t *testing.T) {
	for _, socketState := range []string{"absent", "stale", "reachable"} {
		for _, mode := range []string{"human", "json", "foreground", "foreground-json", "failure", "failure-json"} {
			t.Run(socketState+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				repo := filepath.Join(root, "repo")
				if err := os.MkdirAll(repo, 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", root)
				t.Setenv("XDG_CONFIG_HOME", root)
				t.Setenv("XDG_DATA_HOME", root)
				t.Setenv("TREEMAN_CONFIG", "")
				t.Setenv("TREEMAN_DB_PATH", filepath.Join(root, "treeman.db"))
				if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
					t.Fatalf("git init: %v: %s", err, output)
				}
				hook := "printf prepared > prepared"
				if strings.Contains(mode, "failure") {
					hook = "exit 17"
				}
				body := "databases: []\nhooks:\n  create-after-engines:\n    - run: " + hook + "\n"
				if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}

				bin := filepath.Join(root, "bin")
				if err := os.MkdirAll(bin, 0o755); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(root, "daemon-started")
				t.Setenv("NO_DAEMON_MARKER", marker)
				script := []byte("#!/bin/sh\nprintf called >> \"$NO_DAEMON_MARKER\"\nexit 1\n")
				for _, name := range []string{"systemctl", "launchctl", "treemand"} {
					if err := os.WriteFile(filepath.Join(bin, name), script, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

				socketPath := "s"
				setupPrepareNoDaemonSocket(t, root, socketPath, socketState)

				var output bytes.Buffer
				old := ui.Out
				ui.Out = &output
				t.Cleanup(func() { ui.Out = old })
				args := []string{"treeman", "prepare", "--no-daemon", "--repo", repo, "--worktree", repo}
				if strings.Contains(mode, "foreground") {
					args = append(args, "--foreground")
				}
				if strings.Contains(mode, "json") {
					args = append(args, "--json")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				app := &cli.Command{Name: "treeman", Commands: []*cli.Command{PrepareCmd()}}
				err := app.Run(ctx, args)
				assertPrepareNoDaemonResult(t, repo, mode, &output, err)
				assertPrepareNoDaemonUntouched(t, marker, socketPath, socketState)
			})
		}
	}
}

func setupPrepareNoDaemonSocket(t *testing.T, root, socketPath, socketState string) {
	t.Helper()
	t.Chdir(root)
	t.Setenv("TREEMAN_SOCKET", socketPath)
	if socketState == "absent" {
		return
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if socketState == "stale" {
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}

	var connections atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_, _ = conn.Write([]byte("{\"kind\":\"error\",\"message\":\"unexpected daemon connection\"}\n"))
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		if got := connections.Load(); got != 0 {
			t.Errorf("made %d daemon connections", got)
		}
	})
}

func assertPrepareNoDaemonResult(t *testing.T, repo, mode string, output *bytes.Buffer, runErr error) {
	t.Helper()
	if strings.Contains(mode, "failure") {
		if runErr == nil || !strings.Contains(runErr.Error(), "17") {
			t.Errorf("expected hook failure, got %v", runErr)
		}
		if output.Len() != 0 {
			t.Errorf("unexpected success output on failure: %s", output)
		}
		return
	}
	if runErr != nil {
		t.Fatalf("prepare: %v; output: %s", runErr, output)
	}
	if content, err := os.ReadFile(filepath.Join(repo, "prepared")); err != nil || string(content) != "prepared" {
		t.Fatalf("inline post-engine hook did not finish: %q, %v", content, err)
	}
	if strings.Contains(mode, "json") {
		var payload struct {
			Outcomes []json.RawMessage `json:"outcomes"`
		}
		if err := json.Unmarshal(output.Bytes(), &payload); err != nil || len(payload.Outcomes) != 0 {
			t.Errorf("unexpected JSON: %s (%v)", output, err)
		}
		return
	}
	if !strings.Contains(output.String(), "prepare complete") {
		t.Errorf("missing completion: %s", output)
	}
}

func assertPrepareNoDaemonUntouched(t *testing.T, marker, socketPath, socketState string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("daemon startup attempted: %v", err)
	}
	if socketState == "absent" {
		if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
			t.Errorf("daemon socket created: %v", err)
		}
		return
	}
	if info, err := os.Stat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Errorf("existing socket removed or replaced: %v", err)
	}
}
