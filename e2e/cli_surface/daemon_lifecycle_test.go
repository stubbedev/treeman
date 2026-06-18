//go:build e2e

// Daemon lifecycle CLI coverage: `treeman daemon start` / `status` /
// `reload` / `stop` driven against a REAL treemand process, fully
// isolated (own socket, DB, HOME, XDG dirs). The other daemon CLI
// tests (admin_test.go) only exercise the no-daemon error paths; this
// is the one that proves the start→status→reload→stop happy path.
//
// `daemon install` / `uninstall` are deliberately NOT covered e2e:
// they run `systemctl --user enable --now` (or launchctl) against the
// real per-user service manager, which a test must not mutate.
package cli_surface_e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	daemonBinOnce sync.Once
	daemonBinDir  string
	daemonBinErr  error
)

// sharedDaemonBinDir compiles treemand once and returns the directory
// holding the binary (so callers can prepend it to PATH — daemonctl
// resolves treemand via exec.LookPath).
func sharedDaemonBinDir(t *testing.T) string {
	t.Helper()
	daemonBinOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			daemonBinErr = err
			return
		}
		repoRoot, err := os.Getwd()
		if err != nil {
			daemonBinErr = err
			return
		}
		repoRoot = filepath.Dir(filepath.Dir(repoRoot))
		dir, err := os.MkdirTemp("", "treemand-e2e-bin-*")
		if err != nil {
			daemonBinErr = err
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "treemand"), "./cmd/treemand")
		cmd.Dir = repoRoot
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			daemonBinErr = err
			return
		}
		daemonBinDir = dir
	})
	if daemonBinErr != nil {
		t.Skipf("treemand build failed / skipped: %v", daemonBinErr)
	}
	return daemonBinDir
}

var pidRe = regexp.MustCompile(`pid (\d+)`)

func TestDaemonStartStatusReloadStop(t *testing.T) {
	binDir := sharedDaemonBinDir(t)
	e := newEnv(t)

	// A short socket path — t.TempDir() embeds the (long) test name and
	// can blow past the ~104-byte sun_path limit when bind()ing.
	sockDir, err := os.MkdirTemp("", "tmd-rt-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "treeman.sock")

	// run with treemand on PATH + the pinned socket. The daemon
	// (spawned by `daemon start` via exec.Command with inherited env)
	// picks up the same TREEMAN_SOCKET, so CLI and daemon agree.
	run := func(args ...string) cliResult {
		cmd := exec.Command(sharedBin(t), args...)
		cmd.Env = append(os.Environ(), e.block()...)
		cmd.Env = append(cmd.Env,
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"TREEMAN_SOCKET="+sock,
		)
		var sout, serr strings.Builder
		cmd.Stdout = &sout
		cmd.Stderr = &serr
		err := cmd.Run()
		return cliResult{stdout: sout.String(), stderr: serr.String(), err: err}
	}

	// start
	startRes := run("daemon", "start")
	if startRes.err != nil {
		t.Fatalf("daemon start failed: %v\nstdout:%s\nstderr:%s", startRes.err, startRes.stdout, startRes.stderr)
	}
	// Best-effort reap so a hung daemon never outlives the test.
	if m := pidRe.FindStringSubmatch(startRes.stdout); m != nil {
		if pid, perr := strconv.Atoi(m[1]); perr == nil {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		}
	}

	// status — poll until the daemon has bound the socket + answers.
	var statusUp bool
	for i := 0; i < 40; i++ {
		res := run("daemon", "status", "--json")
		if res.err == nil && strings.Contains(res.stdout, `"status":"running"`) {
			statusUp = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !statusUp {
		t.Fatalf("daemon never reported running after start")
	}

	// reload (all repos) — exercises the MethodConfigReload RPC path.
	reloadRes := run("daemon", "reload")
	if reloadRes.err != nil {
		t.Errorf("daemon reload failed: %v\nstderr:%s", reloadRes.err, reloadRes.stderr)
	}

	// stop — graceful shutdown via RPC.
	stopRes := run("daemon", "stop")
	if stopRes.err != nil {
		t.Errorf("daemon stop failed: %v\nstderr:%s", stopRes.err, stopRes.stderr)
	}

	// status — poll until it reports not-running (socket gone).
	var down bool
	for i := 0; i < 40; i++ {
		res := run("daemon", "status", "--json")
		if strings.Contains(res.stdout, `"status":"not-running"`) {
			down = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !down {
		t.Errorf("daemon still reported running after stop")
	}
}
