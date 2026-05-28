//go:build e2e

// Package cliengine_e2e exercises the engine-backed CLI commands that run
// inline (no daemon): `treeman prepare`, `treeman db status`, and
// `treeman db reset`, against a real MySQL via the compiled binary.
package cliengine_e2e

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
)

const mysqlAddr = "127.0.0.1:13360"

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

func treemanBin(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		root, err := os.Getwd()
		if err != nil {
			binErr = err
			return
		}
		root = filepath.Dir(filepath.Dir(root))
		// NOT t.TempDir() — the binary is shared across tests via
		// sync.Once, but t.TempDir() is cleaned up when the first
		// caller finishes, deleting the binary out from under later
		// tests. Use a process-lifetime temp dir instead.
		dir, err := os.MkdirTemp("", "treeman-cliengine-bin-*")
		if err != nil {
			binErr = err
			return
		}
		p := filepath.Join(dir, "treeman")
		cmd := exec.Command("go", "build", "-o", p, "./cmd/treeman")
		cmd.Dir = root
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			binErr = err
			return
		}
		binPath = p
	})
	if binErr != nil {
		t.Fatalf("build treeman: %v", binErr)
	}
	return binPath
}

func up(t *testing.T) {
	t.Helper()
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:"+mysqlAddr, 60*time.Second, func() error {
		db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})
}

// runTreeman runs the binary in `repo` with an isolated DB/XDG/HOME.
func runTreeman(t *testing.T, repo string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(treemanBin(t), args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"XDG_STATE_HOME="+t.TempDir(),
		"XDG_DATA_HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+t.TempDir(),
		"TREEMAN_DB_PATH="+filepath.Join(t.TempDir(), "treeman.db"),
		"TREEMAN_NO_PAGER=1", "NO_COLOR=1",
	)
	var sout, serr strings.Builder
	cmd.Stdout, cmd.Stderr = &sout, &serr
	err := cmd.Run()
	return sout.String(), serr.String(), err
}

func TestCLIPrepare(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	up(t)

	repo := gitRepo(t)
	write(t, repo, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2);")
	write(t, repo, ".treeman.yaml", `connections:
  mysql:
    host: 127.0.0.1
    port: 13360
    user: root
    password: rootpw
databases:
  - engine: mysql
    name_template: cli_prep_{slug}
    dump: seed.sql
`)
	commit(t, repo)

	sout, serr, err := runTreeman(t, repo, "prepare")
	if err != nil {
		t.Fatalf("treeman prepare: %v\nstdout:\n%s\nstderr:\n%s", err, sout, serr)
	}
	// The source DB must now exist.
	if !anyDBWithPrefix(t, "cli_prep_") {
		t.Errorf("treeman prepare created no cli_prep_* database\nstdout:\n%s", sout)
	}
}

func TestCLIDbStatusAndReset(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	up(t)

	repo := gitRepo(t)
	write(t, repo, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1);")
	write(t, repo, ".treeman.yaml", `connections:
  mysql:
    host: 127.0.0.1
    port: 13360
    user: root
    password: rootpw
databases:
  - engine: mysql
    name_template: cli_bs_{slug}
    dump: seed.sql
    branch_scoped: true
`)
	commit(t, repo)

	// Prime the branch_scoped active DB.
	if _, serr, err := runTreeman(t, repo, "prepare"); err != nil {
		t.Fatalf("prepare branch_scoped: %v\n%s", err, serr)
	}

	// `db status` reports the branch_scoped database (inline, no daemon).
	sout, serr, err := runTreeman(t, repo, "db", "status")
	if err != nil {
		t.Fatalf("db status: %v\nstderr:\n%s", err, serr)
	}
	if !strings.Contains(sout, "cli_bs_") && !strings.Contains(sout, "branch") {
		t.Errorf("db status output unexpected:\n%s", sout)
	}

	// `db reset` re-syncs the branch_scoped DB from the base (inline).
	// With no upstream it re-seeds from the dump; we just assert it runs
	// cleanly and a subsequent prepare keeps the active DB present.
	if _, serr, err := runTreeman(t, repo, "db", "reset"); err != nil {
		t.Fatalf("db reset: %v\nstderr:\n%s", err, serr)
	}
	if _, serr, err := runTreeman(t, repo, "prepare"); err != nil {
		t.Fatalf("prepare after reset: %v\n%s", err, serr)
	}
	if !anyDBWithPrefix(t, "cli_bs_") {
		t.Errorf("branch_scoped active DB missing after db reset + prepare")
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func anyDBWithPrefix(t *testing.T, prefix string) bool {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ?`, prefix+"%").Scan(&n); err != nil {
		t.Fatalf("schema query: %v", err)
	}
	return n > 0
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	write(t, repo, "README", "hi")
	return repo
}

func commit(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "init")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
