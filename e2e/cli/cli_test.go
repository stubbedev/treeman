//go:build e2e

// Package cli_e2e drives the actual `treeman` + `treemand` binaries
// end-to-end. This is the "full stack" test — RPC over the unix
// socket, real worktree creation via `git worktree add`, real Links
// and Copies bring-in, real Patches apply. The other e2e suites
// shortcut via the prepare/daemon Go API; this one is the canonical
// user-experience test.
//
// Exercises in one run:
//   - `treeman init` produces a parseable .treeman.yaml
//   - `treeman wt create <branch>` creates the worktree
//   - worktree.Links symlinks resolve correctly
//   - worktree.Copies files are duplicated
//   - patches block rewrites .env-style file
//   - patches.skip_worktree applies git update-index
//   - engine prepare completes (DB exists with expected table)
//   - `treeman wt delete` tears down DB + clones + git worktree
package cli_e2e

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

func TestCLIEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13406", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13406", 1*time.Second)
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

	// Put the freshly-built `treeman` on PATH. The patches clean/smudge
	// filter is wired as the bare program `treeman patch-filter` (a
	// documented contract — see internal/patcher/install.go), and it's
	// `required=true`, so every git op in a patched worktree must be able
	// to resolve it. Both the daemon (spawned below, inherits os.Environ)
	// and this test's own `git` invocations rely on this. Without it git
	// can't run the clean filter and patched files show as modified.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Per-test sockets + state so we don't collide with the
	// developer's running daemon.
	runtimeDir := t.TempDir()
	stateDir := t.TempDir()
	socket := filepath.Join(runtimeDir, "treeman.sock")
	// Isolate the daemon's state DB. DefaultDBPath resolves to
	// $TREEMAN_DB_PATH → $XDG_DATA_HOME/treeman → ~/.local/share/treeman —
	// it does NOT consult XDG_STATE_HOME, so without this the test daemon
	// opens the developer's REAL shared DB: it then contends with a running
	// treemand for the SQLite write lock (stalling the detached teardown
	// past the 30s wait) and accumulates stale repo rows across runs.
	dbPath := filepath.Join(t.TempDir(), "treeman.db")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TREEMAN_DB_PATH", dbPath)

	// Start treemand in the background.
	daemonCmd := exec.Command(filepath.Join(binDir, "treemand"))
	daemonCmd.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+runtimeDir,
		"XDG_STATE_HOME="+stateDir,
		"TREEMAN_DB_PATH="+dbPath,
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
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon stderr (tail):\n%s", string(daemonStderr.buf))
		}
	})
	// Wait for the socket to appear.
	harness.WaitForReady(t, "treemand-socket", 10*time.Second, func() error {
		_, err := os.Stat(socket)
		return err
	})

	// Build a git repo with .treeman.yaml + Links/Copies fixtures.
	mainRepo := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, mainRepo, "init", "-q", "-b", "main")
	mustGit(t, mainRepo, "config", "user.email", "e2e@example.com")
	mustGit(t, mainRepo, "config", "user.name", "e2e")

	// Fixture: a "vendor" dir to symlink in, a ".env" file to copy in,
	// a phpunit.xml to patch.
	must := func(path, body string) {
		full := filepath.Join(mainRepo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(".env", "MYSQL_PW=rootpw\nDB_DATABASE=ignored\n")
	must(".gitignore", "vendor/\n")
	must("phpunit.xml", `<?xml version="1.0"?><phpunit><php><env name="DB_DATABASE" value="placeholder"/></php></phpunit>`)
	must("seed.sql", "CREATE TABLE widgets (id INT PRIMARY KEY); INSERT INTO widgets VALUES (1),(2);")
	must(".treeman.yaml", `
worktrees:
  root: .worktrees
  copies:
    - .env
  links:
    - vendor
env_sources:
  - .env
patches:
  - file: phpunit.xml
    format: phpunit
    skip_worktree: false
    set:
      DB_DATABASE: app_{slug}
connections:
  mysql:
    host: 127.0.0.1
    port: 13406
    user: root
    password: $MYSQL_PW
databases:
  - engine: mysql
    name_template: tm_cli_{slug}
    dump: seed.sql
`)
	mustGit(t, mainRepo, "add", "-A")
	mustGit(t, mainRepo, "commit", "-q", "-m", "init")
	// Create the gitignored vendor dir AFTER commit so it lives in
	// the main worktree only — same as a developer running
	// `composer install` after cloning.
	must("vendor/lib.txt", "shared vendor cache")

	// `treeman wt create <branch>` — full pipeline.
	wtBranch := "feature/test"
	out := runTreeman(t, binDir, mainRepo, "wt", "create", wtBranch)
	t.Logf("wt create output:\n%s", out)

	wtPath := filepath.Join(mainRepo, ".worktrees", "feature/test")
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree dir not created: %v", err)
	}

	// ── Links ──: vendor should be a symlink to main repo's vendor.
	vendor := filepath.Join(wtPath, "vendor")
	li, err := os.Lstat(vendor)
	if err != nil {
		t.Errorf("vendor symlink missing: %v", err)
	} else if li.Mode()&os.ModeSymlink == 0 {
		t.Errorf("vendor should be a symlink, got mode %v", li.Mode())
	}

	// ── Copies ──: .env should be a regular file.
	envCopy := filepath.Join(wtPath, ".env")
	ei, err := os.Lstat(envCopy)
	if err != nil {
		t.Errorf(".env copy missing: %v", err)
	} else if ei.Mode()&os.ModeSymlink != 0 {
		t.Errorf(".env should be a copy, not a symlink")
	}

	// ── Patches ──: phpunit.xml should now reference app_<slug>.
	phpunit, err := os.ReadFile(filepath.Join(wtPath, "phpunit.xml"))
	if err != nil {
		t.Errorf("phpunit.xml missing: %v", err)
	} else if !strings.Contains(string(phpunit), `value="app_`) {
		t.Errorf("phpunit.xml not patched: %s", phpunit)
	}

	// ── Patches don't trip git dirty checks ──: the tracked, per-worktree-
	// rewritten phpunit.xml must NOT show as modified. `treeman wt create`
	// wired the real clean filter (`treeman patch-filter clean`, via
	// EnsureFilter); `git status` runs it and sees content == HEAD, so the
	// worktree stays clean despite the on-disk rewrite. A regression here
	// (clean filter not byte-stable, or filter not installed) surfaces as
	// " M phpunit.xml" and blocks `git pull`/`checkout`.
	porcelain := gitStatusPorcelain(t, wtPath)
	if strings.Contains(porcelain, "phpunit.xml") {
		t.Errorf("patched phpunit.xml shows as a git modification (clean filter not hiding the per-worktree rewrite):\n%s", porcelain)
	}
	if strings.TrimSpace(porcelain) != "" {
		t.Logf("worktree git status (non-fatal, for context):\n%s", porcelain)
	}

	// ── Engine prepare ──: poll for the source DB to appear
	// (FinalizeWorktree runs async, so the DB might not exist
	// immediately).
	harness.WaitForReady(t, "source-db", 30*time.Second, func() error {
		dbs := listDatabases(t)
		for _, d := range dbs {
			if strings.HasPrefix(d, "tm_cli_") {
				return nil
			}
		}
		return fmt.Errorf("no tm_cli_* database yet: %v", dbs)
	})
	dbs := listDatabases(t)
	var sourceDB string
	for _, d := range dbs {
		if strings.HasPrefix(d, "tm_cli_") {
			sourceDB = d
			break
		}
	}
	t.Logf("source DB: %s", sourceDB)
	assertCount(t, sourceDB, "widgets", 2)

	// ── wt delete ──: should drop the DB AND remove the worktree.
	out = runTreeman(t, binDir, mainRepo, "wt", "delete", "--yes", wtBranch)
	t.Logf("wt delete output:\n%s", out)

	harness.WaitForReady(t, "drop-source-db", 30*time.Second, func() error {
		for _, d := range listDatabases(t) {
			if d == sourceDB {
				return fmt.Errorf("source still present: %s", sourceDB)
			}
		}
		return nil
	})
	// `wt delete` detaches teardown + DB teardown + git-remove to the daemon;
	// the git-remove of the worktree dir is a separate async step from the DB
	// drop above, so poll for it rather than asserting once (else a slow
	// git-remove races the DB-drop readiness and flakes).
	harness.WaitForReady(t, "remove-worktree-dir", 30*time.Second, func() error {
		if _, err := os.Stat(wtPath); err == nil {
			return fmt.Errorf("worktree dir still exists after delete: %s", wtPath)
		}
		return nil
	})
}

// TestCLIPrintPathStreamDiscipline verifies the shell-shim contract:
//
//	cd "$(treeman wt create x --print-path)"
//
// must work. The worktree path is the ONLY line on stdout; every
// status line ("created worktree #N …", "queued: …", patch
// announcements, etc.) must go to stderr so the cd substitution
// never picks up a non-path line. No engine needed — --skip-hooks
// short-circuits the daemon dispatch.
func TestCLIPrintPathStreamDiscipline(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot = filepath.Dir(filepath.Dir(repoRoot))
	binDir := t.TempDir()
	buildBin(t, binDir, repoRoot, "treeman", "./cmd/treeman")

	// Isolated state so we don't trip over the developer's daemon.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "treeman.db"))

	mainRepo := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, mainRepo, "init", "-q", "-b", "main")
	mustGit(t, mainRepo, "config", "user.email", "e2e@example.com")
	mustGit(t, mainRepo, "config", "user.name", "e2e")
	mustGit(t, mainRepo, "commit", "--allow-empty", "-q", "-m", "initial")

	// Capture stdout + stderr separately so we can assert the split.
	cmd := exec.Command(filepath.Join(binDir, "treeman"),
		"wt", "create", "feature/printpath", "--skip-hooks", "--print-path")
	cmd.Dir = mainRepo
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("treeman wt create: %v\nstdout:\n%s\nstderr:\n%s",
			err, stdout.String(), stderr.String())
	}

	stdoutLines := splitNonEmpty(stdout.String())
	if len(stdoutLines) != 1 {
		t.Fatalf("stdout should be exactly one line (the path); got %d:\n%s",
			len(stdoutLines), stdout.String())
	}
	wtPath := stdoutLines[0]
	if !filepath.IsAbs(wtPath) {
		t.Errorf("stdout line should be an absolute path, got %q", wtPath)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Errorf("printed path does not exist: %v", err)
	}

	// Stream-discipline checks: stdout must NOT carry status text.
	for _, marker := range []string{"created worktree", "queued:", "patched"} {
		if strings.Contains(stdout.String(), marker) {
			t.Errorf("stdout leaked status marker %q:\n%s", marker, stdout.String())
		}
	}
	// And stderr SHOULD carry the success line.
	if !strings.Contains(stderr.String(), "created worktree") {
		t.Errorf("stderr missing 'created worktree' status line:\n%s", stderr.String())
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func buildBin(t *testing.T, binDir, repoRoot, name, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o",
		filepath.Join(binDir, name), pkg)
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
}

// gitStatusPorcelain returns `git status --porcelain` for dir. Used to
// assert that treeman's per-worktree patches stay hidden from git's
// dirty checks via the installed clean filter.
func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status --porcelain: %v\n%s", err, out)
	}
	return string(out)
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

func listDatabases(t *testing.T) []string {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp(127.0.0.1:13406)/")
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

func assertCount(t *testing.T, dbName, table string, want int) {
	t.Helper()
	db, err := sql.Open("mysql",
		fmt.Sprintf("root:rootpw@tcp(127.0.0.1:13406)/%s", dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if n != want {
		t.Errorf("%s.%s rows = %d, want %d", dbName, table, n, want)
	}
}

// lineBuf is a minimal io.Writer that keeps the most recent few KB
// for diagnostics on failure.
type lineBuf struct{ buf []byte }

func (b *lineBuf) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > 4096 {
		b.buf = b.buf[len(b.buf)-4096:]
	}
	return len(p), nil
}
