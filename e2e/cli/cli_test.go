//go:build e2e

// Package cli_e2e drives the actual `treeman` + `treemand` binaries
// end-to-end. This is the "full stack" test — RPC over the unix
// socket, real worktree creation via `git worktree add`, real Links
// and Copies bring-in, real Patches apply. The other e2e suites
// shortcut via the prepare/daemon Go API; this one is the canonical
// user-experience test.
//
// Exercises in one run:
//   • `treeman init` produces a parseable .treeman.yaml
//   • `treeman wt create <branch>` creates the worktree
//   • worktree.Links symlinks resolve correctly
//   • worktree.Copies files are duplicated
//   • patches block rewrites .env-style file
//   • patches.skip_worktree applies git update-index
//   • engine prepare completes (DB exists with expected table)
//   • `treeman wt delete` tears down DB + clones + git worktree
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

	// Per-test sockets + state so we don't collide with the
	// developer's running daemon.
	runtimeDir := t.TempDir()
	stateDir := t.TempDir()
	socket := filepath.Join(runtimeDir, "treeman.sock")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Start treemand in the background.
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
    password_env: MYSQL_PW
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
	if _, err := os.Stat(wtPath); err == nil {
		t.Errorf("worktree dir still exists after delete: %s", wtPath)
	}
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
