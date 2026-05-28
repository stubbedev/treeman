//go:build e2e

// Package mainworktreedb_e2e proves the main_worktree.databases overlay
// reaches the REAL database, not just the hook env (which the no-docker
// e2e/main_worktree suite already covers). Against a real MySQL it
// asserts that, for the same repo:
//
//   - the MAIN worktree's source DB uses the overlay's name_template AND
//     the overlay's `test_clones: {clones: 0}` — so it pre-warms ZERO
//     clones (fanout disabled in main), and
//   - a LINKED worktree (base config, no overlay) uses the base
//     name_template AND base `test_clones: {clones: 2}` — so it DOES
//     pre-warm 2 clones.
//
// This is the overlay's headline use case: the repo-root checkout runs a
// single app DB while linked worktrees fan out paratest clones.
package mainworktreedb_e2e

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

const mysqlAddr = "127.0.0.1:13342"

func TestMainWorktreeOverlayHitsRealDB(t *testing.T) {
	harness.SkipIfNoDocker(t)
	requireGit(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:"+mysqlAddr, 60*time.Second, func() error {
		db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	})

	ctx := context.Background()
	repoRoot := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "develop", repoRoot)
	mustGit(t, repoRoot, "config", "user.email", "t@t")
	mustGit(t, repoRoot, "config", "user.name", "t")
	writeFile(t, repoRoot, "README", "hi")
	writeFile(t, repoRoot, "seed.sql", "CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2);")
	// Base: 2 paratest clones. Overlay (main only): a different
	// name_template and fanout disabled (clones: 0).
	writeFile(t, repoRoot, ".treeman.yaml", `
main_worktree:
  enabled: true
  databases:
    - name_template: "tmmwmain_{slug}"
      test_clones:
        clones: 0
        name_template: ""
connections:
  mysql:
    host: 127.0.0.1
    port: 13342
    user: root
    password: rootpw
databases:
  - engine: mysql
    name_template: "tmmwbase_{slug}"
    dump: seed.sql
    test_clones:
      clones: 2
      name_template: "tmmwbase_{slug}_w{n}"
`)
	mustGit(t, repoRoot, "add", "-A")
	mustGit(t, repoRoot, "commit", "-q", "-m", "init")

	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	state := daemon.NewState(ctx, st)
	env := map[string]string{"PATH": os.Getenv("PATH")}

	// ── Main worktree: overlay applies (name + clones:0) ──
	if _, err := daemon.EnrollMainWorktree(ctx, state, repoRoot); err != nil {
		t.Fatalf("enroll main: %v", err)
	}
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env); err != nil {
		t.Fatalf("finalize main: %v", err)
	}

	// Main slug is main_<branch> → overlay renders tmmwmain_main_develop.
	const mainDB = "tmmwmain_main_develop"
	if !dbExists(t, mainDB) {
		t.Errorf("main source DB %s missing — overlay name_template did not reach the real DB (have: %v)",
			mainDB, listLike(t, "tmmwmain_%"))
	}
	if _, mainClones := partition(listLike(t, "tmmwmain_%")); len(mainClones) != 0 {
		t.Errorf("overlay test_clones:0 should disable fanout for main, but found clones: %v", mainClones)
	}

	// ── Linked worktree: base config applies (base name + 2 clones) ──
	wtPath := filepath.Join(repoRoot, ".worktrees", "feature-x")
	mustGit(t, repoRoot, "worktree", "add", "-q", "-b", "feature/x", wtPath)
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, wtPath, env); err != nil {
		t.Fatalf("finalize linked: %v", err)
	}

	// Linked uses the base name_template (tmmwbase_<pathslug>) — distinct
	// from the overlay prefix — and pre-warms exactly 2 clones.
	baseSources, baseClones := partition(listLike(t, "tmmwbase_%"))
	if len(baseSources) == 0 {
		t.Errorf("linked source DB (base name_template) missing (have: %v)", listLike(t, "tmmwbase_%"))
	}
	if len(baseClones) != 2 {
		t.Errorf("base test_clones:2 should pre-warm 2 clones for the linked worktree, got %d: %v",
			len(baseClones), baseClones)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func dbExists(t *testing.T, name string) bool {
	t.Helper()
	db := openMySQL(t)
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&n); err != nil {
		t.Fatalf("schema check %s: %v", name, err)
	}
	return n == 1
}

// listLike returns schema names matching the LIKE pattern (caller may
// pass an escaped `\_` to match a literal underscore).
func listLike(t *testing.T, pattern string) []string {
	t.Helper()
	db := openMySQL(t)
	defer db.Close()
	rows, err := db.QueryContext(context.Background(),
		`SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE ? ESCAPE '\\'`, pattern)
	if err != nil {
		t.Fatalf("list like %q: %v", pattern, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// partition splits schema names into sources vs clones. A clone's final
// `_`-segment is `w<digits>` (the `_w{n}` paratest suffix); everything
// else is a source. Robust against path-based slugs like `wt_<hex>`,
// where a naive `%_w%` LIKE would false-match the `_wt` in the slug.
func partition(names []string) (sources, clones []string) {
	for _, s := range names {
		tail := s[strings.LastIndex(s, "_")+1:]
		if len(tail) >= 2 && tail[0] == 'w' && allDigits(tail[1:]) {
			clones = append(clones, s)
		} else {
			sources = append(sources, s)
		}
	}
	return sources, clones
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func openMySQL(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp("+mysqlAddr+")/")
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	return db
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
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

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
