//go:build e2e

// Package race_e2e probes the cleanup-before-finalize race:
// `treeman wt delete` dispatched while the daemon's async finalize
// (setup hooks + engine prepare + fanout clones) is still in flight.
//
// The contract under test: after both operations settle, *nothing*
// must remain — no worktree directory, no registry row, no databases
// matching the worktree's slug. Anything left over means the cleanup
// path failed to wait on (or supersede) the in-flight finalize.
package race_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
)

const (
	composeDir = "."
	mysqlPort  = 13506
	mysqlDSN   = "root:rootpw@tcp(127.0.0.1:13506)/"
)

// TestDeleteDuringFinalize reproduces the race where wt delete fires
// while the daemon's finalize goroutine is still running setup hooks
// + prepare + fanout. End-state must be fully clean.
func TestDeleteDuringFinalize(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(composeDir)))
	harness.WaitForReady(t, fmt.Sprintf("mysql:%d", mysqlPort), 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", mysqlPort), time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	// Build fresh binaries.
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot = filepath.Dir(filepath.Dir(repoRoot)) // up to project root
	binDir := t.TempDir()
	buildBin(t, binDir, repoRoot, "treeman", "./cmd/treeman")
	buildBin(t, binDir, repoRoot, "treemand", "./cmd/treemand")

	runtimeDir := t.TempDir()
	stateDir := t.TempDir()
	socket := filepath.Join(runtimeDir, "treeman.sock")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	daemonCmd := exec.Command(filepath.Join(binDir, "treemand"))
	daemonCmd.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+runtimeDir,
		"XDG_STATE_HOME="+stateDir,
	)
	daemonStderr := &lineBuf{}
	daemonCmd.Stderr = daemonStderr
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("treemand start: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Kill()
		_, _ = daemonCmd.Process.Wait()
	})
	harness.WaitForReady(t, "treemand-socket", 10*time.Second, func() error {
		_, err := os.Stat(socket)
		return err
	})

	mainRepo := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, mainRepo, "init", "-q", "-b", "main")
	mustGit(t, mainRepo, "config", "user.email", "e2e@example.com")
	mustGit(t, mainRepo, "config", "user.name", "e2e")

	must := func(rel, body string) {
		full := filepath.Join(mainRepo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Hook is intentionally slow: 6 seconds of cumulative work
	// stretches finalize well past the wt-delete dispatch so the
	// teardown lands while setup is still running. Each subcommand
	// sleeps 2s so SIGTERM mid-hook still leaves something running.
	must(".treeman.yaml", `
worktrees:
  root: .worktrees
hooks:
  create-before-engines:
    - run: sleep 2
    - run: sleep 2
    - run: sleep 2
connections:
  mysql:
    host: 127.0.0.1
    port: 13506
    user: root
    password: rootpw
databases:
  - engine: mysql
    name_template: tm_race_{slug}
    dump: seed.sql
`)
	must("seed.sql", "CREATE TABLE widgets (id INT PRIMARY KEY); INSERT INTO widgets VALUES (1);")
	mustGit(t, mainRepo, "add", "-A")
	mustGit(t, mainRepo, "commit", "-q", "-m", "init")

	branch := "feature/raceme"
	out := runTreeman(t, binDir, mainRepo, "wt", "create", branch)
	t.Logf("wt create stdout:\n%s", out)
	wtPath := filepath.Join(mainRepo, ".worktrees", "feature/raceme")
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}

	// Window: wt create's daemon-dispatch returns the moment the
	// goroutine starts. Give finalize ~500ms to enter the hook stage
	// so the teardown lands DURING setup, not before it began. Then
	// dispatch delete and measure end-state.
	time.Sleep(500 * time.Millisecond)
	out = runTreeman(t, binDir, mainRepo, "wt", "delete", "--yes", branch)
	t.Logf("wt delete stdout:\n%s", out)

	// Both operations are daemon-dispatched and run concurrently.
	// Give the slowest path (full 6s hook chain + cleanup) ample
	// budget to settle before asserting clean end-state.
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	start := time.Now()
	for time.Now().Before(deadline) {
		lastErr = assertFullyCleaned(t, binDir, mainRepo, wtPath, "tm_race_")
		if lastErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("end-state not clean after 60s: %v", lastErr)
	}
	t.Logf("verified clean end-state after %s", time.Since(start))

	// Finalize may still be running (slow hooks ignore the missing
	// worktree dir). Wait the full hook budget + a margin, then
	// re-assert: a late-arriving finalize must NOT resurrect the
	// worktree, recreate the DB, or insert a fresh registry row.
	time.Sleep(10 * time.Second)
	if err := assertFullyCleaned(t, binDir, mainRepo, wtPath, "tm_race_"); err != nil {
		t.Fatalf("late-arriving finalize resurrected state: %v", err)
	}
	t.Logf("verified clean end-state still holds after 10s settle")

	// Dump the daemon-side event timeline to surface useful failure
	// context whenever this test misbehaves on a future run.
	t.Logf("event timeline:\n%s",
		runTreemanCapture(t, binDir, mainRepo, "logs", "tail", "--all", "-n", "200"))
}

// TestDeleteImmediatelyAfterCreate is the tighter variant: no sleep
// between `wt create` and `wt delete`. The two dispatches reach the
// daemon back-to-back; finalize may not yet have entered its first
// phase when teardown fires. The cancellation path must still leave
// no residue (no DB, no row, no dir).
func TestDeleteImmediatelyAfterCreate(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(composeDir)))
	harness.WaitForReady(t, fmt.Sprintf("mysql:%d", mysqlPort), 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", mysqlPort), time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot = filepath.Dir(filepath.Dir(repoRoot))
	binDir := t.TempDir()
	buildBin(t, binDir, repoRoot, "treeman", "./cmd/treeman")
	buildBin(t, binDir, repoRoot, "treemand", "./cmd/treemand")

	runtimeDir := t.TempDir()
	stateDir := t.TempDir()
	socket := filepath.Join(runtimeDir, "treeman.sock")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	daemonCmd := exec.Command(filepath.Join(binDir, "treemand"))
	daemonCmd.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+runtimeDir,
		"XDG_STATE_HOME="+stateDir,
	)
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("treemand start: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Kill()
		_, _ = daemonCmd.Process.Wait()
	})
	harness.WaitForReady(t, "treemand-socket", 10*time.Second, func() error {
		_, err := os.Stat(socket)
		return err
	})

	mainRepo := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, mainRepo, "init", "-q", "-b", "main")
	mustGit(t, mainRepo, "config", "user.email", "e2e@example.com")
	mustGit(t, mainRepo, "config", "user.name", "e2e")

	must := func(rel, body string) {
		full := filepath.Join(mainRepo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// No hooks this time — finalize jumps straight to prepare. Makes
	// the window between dispatch and DB-creation as tight as the
	// daemon goroutine scheduler allows.
	must(".treeman.yaml", `
worktrees:
  root: .worktrees
connections:
  mysql:
    host: 127.0.0.1
    port: 13506
    user: root
    password: rootpw
databases:
  - engine: mysql
    name_template: tm_race_imm_{slug}
    dump: seed.sql
`)
	must("seed.sql", "CREATE TABLE widgets (id INT PRIMARY KEY);")
	mustGit(t, mainRepo, "add", "-A")
	mustGit(t, mainRepo, "commit", "-q", "-m", "init")

	branch := "feature/raceimm"
	out := runTreeman(t, binDir, mainRepo, "wt", "create", branch)
	t.Logf("wt create stdout:\n%s", out)
	wtPath := filepath.Join(mainRepo, ".worktrees", "feature/raceimm")

	// No sleep — fire delete immediately. Cancellation may catch
	// finalize before EnsureWorktree, mid-prepare, or after prepare.
	// In all three cases the eventual end-state must be clean.
	out = runTreeman(t, binDir, mainRepo, "wt", "delete", "--yes", branch)
	t.Logf("wt delete stdout:\n%s", out)

	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = assertFullyCleaned(t, binDir, mainRepo, wtPath, "tm_race_imm_")
		if lastErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("end-state not clean: %v", lastErr)
	}
	time.Sleep(5 * time.Second)
	if err := assertFullyCleaned(t, binDir, mainRepo, wtPath, "tm_race_imm_"); err != nil {
		t.Fatalf("late finalize resurrected state: %v", err)
	}
	t.Logf("event timeline:\n%s",
		runTreemanCapture(t, binDir, mainRepo, "logs", "tail", "--all", "-n", "200"))
}

// assertFullyCleaned reports nil only when every artifact the
// race could leave behind is gone:
//   - the worktree directory on disk
//   - the active registry row (or, more strictly, the active list
//     contains no row at this path)
//   - any DB whose name starts with the worktree-slug prefix
func assertFullyCleaned(t *testing.T, binDir, mainRepo, wtPath, dbPrefix string) error {
	t.Helper()
	if _, err := os.Stat(wtPath); err == nil {
		return fmt.Errorf("worktree dir still exists: %s", wtPath)
	}
	out := runTreemanCapture(t, binDir, mainRepo, "wt", "list", "--json")
	if strings.Contains(out, wtPath) {
		return fmt.Errorf("registry still lists worktree path:\n%s", out)
	}
	dbs := listDatabases(t)
	for _, d := range dbs {
		if strings.HasPrefix(d, dbPrefix) {
			return fmt.Errorf("database %q with prefix %q still present (all=%v)", d, dbPrefix, dbs)
		}
	}
	return nil
}

// ── helpers ─────────────────────────────────────────────────────

func buildBin(t *testing.T, binDir, repoRoot, name, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, name), pkg)
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func runTreeman(t *testing.T, binDir, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "treeman"), args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("treeman %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// runTreemanCapture mirrors runTreeman but tolerates non-zero exit so
// assertion polling can read state even when a transient error fires.
func runTreemanCapture(t *testing.T, binDir, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "treeman"), args...)
	cmd.Dir = cwd
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func listDatabases(t *testing.T) []string {
	t.Helper()
	db, err := sql.Open("mysql", mysqlDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SHOW DATABASES")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

type lineBuf struct{ buf []byte }

func (b *lineBuf) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > 4096 {
		b.buf = b.buf[len(b.buf)-4096:]
	}
	return len(p), nil
}
