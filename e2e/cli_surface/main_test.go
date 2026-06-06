//go:build e2e

// Package cli_surface_e2e exercises every non-engine `treeman` subcommand
// against the real binary. Engine-dependent paths (`prepare`, MySQL /
// Postgres / Mongo / Redis flows) live in their own e2e suites — this
// file is what catches regressions in the CLI plumbing itself: flag
// parsing, exit codes, JSON shape, scope resolution, and the
// metadata-only command surface (init / slug / fw / config / schema /
// doctor / registry / snapshots / hook run / daemon status / wt
// register|unregister|list|show|switch|back|resolve|prev|wait|logs).
//
// Tests share a single compiled binary (built in TestMain) but each
// gets its own isolated DB / HOME / XDG dirs so they don't pollute
// each other.
package cli_surface_e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

var (
	binPathOnce sync.Once
	binPath     string
	binErr      error
)

// sharedBin compiles the treeman binary once per test process and
// returns the absolute path. Subtests skip when go isn't on PATH.
func sharedBin(t *testing.T) string {
	t.Helper()
	binPathOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			binErr = fmt.Errorf("go not on PATH")
			return
		}
		repoRoot, err := os.Getwd()
		if err != nil {
			binErr = err
			return
		}
		repoRoot = filepath.Dir(filepath.Dir(repoRoot)) // up to project root
		dir, err := os.MkdirTemp("", "treeman-e2e-bin-*")
		if err != nil {
			binErr = err
			return
		}
		p := filepath.Join(dir, "treeman")
		cmd := exec.Command("go", "build", "-o", p, "./cmd/treeman")
		cmd.Dir = repoRoot
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			binErr = fmt.Errorf("build: %w", err)
			return
		}
		binPath = p
	})
	if binErr != nil {
		t.Skipf("binary build failed / skipped: %v", binErr)
	}
	return binPath
}

// env returns a clean environment block isolating XDG dirs, HOME, and
// the SQLite DB. Callers pass extra KEY=VALUE strings to override or
// add to the block.
type env struct {
	home       string
	stateDir   string
	dataDir    string
	configDir  string
	runtimeDir string
	dbPath     string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		home:       t.TempDir(),
		stateDir:   t.TempDir(),
		dataDir:    t.TempDir(),
		configDir:  t.TempDir(),
		runtimeDir: t.TempDir(),
		dbPath:     filepath.Join(t.TempDir(), "treeman.db"),
	}
	// A command like `wt delete` auto-spawns the daemon to run an async
	// teardown; left running, it keeps writing the SQLite DB / sockets
	// into these temp dirs and races t.TempDir's RemoveAll cleanup
	// ("directory not empty"). Registered here so it runs before this
	// env's temp-dir removals (cleanups are LIFO).
	t.Cleanup(func() { stopDaemon(t, e) })
	return e
}

// stopDaemon requests a graceful daemon shutdown and waits until status
// reports not-running (the process has exited and released the DB +
// socket), so a daemon spawned during the test can't keep writing into
// the temp dirs while they're being removed. Best-effort: when no daemon
// was spawned, `daemon status` reports not-running immediately.
func stopDaemon(t *testing.T, e *env) {
	t.Helper()
	_ = e.run(t, e.home, "daemon", "stop")
	for range 50 {
		res := e.run(t, e.home, "daemon", "status", "--json")
		if strings.Contains(res.stdout, `"status":"not-running"`) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *env) block() []string {
	return []string{
		"HOME=" + e.home,
		"XDG_STATE_HOME=" + e.stateDir,
		"XDG_DATA_HOME=" + e.dataDir,
		"XDG_CONFIG_HOME=" + e.configDir,
		"XDG_RUNTIME_DIR=" + e.runtimeDir,
		"TREEMAN_DB_PATH=" + e.dbPath,
		"TREEMAN_SOCKET=" + filepath.Join(filepath.Dir(e.dbPath), "tm.sock"),
		"TREEMAN_NO_PAGER=1",
		"NO_COLOR=1",
	}
}

type cliResult struct {
	stdout string
	stderr string
	err    error
}

func (e *env) run(t *testing.T, cwd string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(sharedBin(t), args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), e.block()...)
	var sout, serr strings.Builder
	cmd.Stdout = &sout
	cmd.Stderr = &serr
	err := cmd.Run()
	return cliResult{stdout: sout.String(), stderr: serr.String(), err: err}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=e2e",
		"GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=e2e",
		"GIT_COMMITTER_EMAIL=e2e@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newGitRepo creates a temp git repo with a single initial commit and
// returns its path. Skips the calling test if git isn't on PATH.
func newGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "e2e@example.com")
	mustGit(t, dir, "config", "user.name", "e2e")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// openStore opens the SQLite file behind env.dbPath. Callers use this
// to seed fixtures (events / hook_runs / worktrees) the binary will
// then read.
func openStore(t *testing.T, e *env) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), e.dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
