//go:build e2e

// Package teardown_e2e verifies what TeardownWorktree actually
// removes from the engine. Specifically:
//
//   - Source DB and all paratest clone DBs are DROPPED.
//   - The fingerprint-keyed TEMPLATE remains so the next worktree
//     with the same inputs hits the cache.
//
// This is the production guarantee — `treeman wt delete` must clean
// up per-worktree state without invalidating the shared cache.
package teardown_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

func TestTeardownDropsSourceAndClonesButKeepsTemplate(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13386", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13386", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "seed.sql"),
		[]byte("CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2),(3);"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".env"),
		[]byte("MYSQL_PW=rootpw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	yaml := `
worktrees:
  root: .worktrees
env_sources: [.env]
connections:
  mysql:
    host: 127.0.0.1
    port: 13386
    user: root
    password: $MYSQL_PW
databases:
  - engine: mysql
    name_template: tm_td_{slug}
    dump: seed.sql
    test_clones:
      clones: 3
      name_template: tm_td_{slug}_w{n}
`
	if err := os.WriteFile(filepath.Join(repoRoot, ".treeman.yaml"),
		[]byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".git/HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := daemon.NewState(ctx, st)
	env := map[string]string{
		"PATH":     os.Getenv("PATH"),
		"MYSQL_PW": "rootpw",
	}

	// ── 1. Finalize → source + 3 clones + 1 template populated ──
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env, false); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	allDBs := listDatabases(t)
	// Clones are named tm_td_<slug>_wN for N>=1; the source is just
	// tm_td_<slug>. Use a regex-ish suffix match.
	source, clones := splitSourceAndClones(allDBs, "tm_td_")
	tmpl := pickByPrefix(allDBs, "_tm_", "")
	t.Logf("after finalize: source=%s clones=%v template=%s", source, clones, tmpl)
	if source == "" {
		t.Fatalf("source DB not created (have: %v)", allDBs)
	}
	if len(clones) != 3 {
		t.Errorf("clone count = %d, want 3 (have: %v)", len(clones), clones)
	}
	if tmpl == "" {
		t.Errorf("template DB not created (have: %v)", allDBs)
	}

	// ── 2. Teardown → source + clones DROPPED, template SURVIVES ──
	if err := daemon.TeardownWorktree(ctx, state, repoRoot, repoRoot, true, env); err != nil {
		// `git worktree remove` will fail against our fake .git
		// layout — that's downstream of the DB drop step, so ignore.
		if !strings.Contains(err.Error(), "git") &&
			!strings.Contains(err.Error(), "worktree remove") {
			t.Fatalf("teardown: %v", err)
		}
	}

	allDBs2 := listDatabases(t)
	if has(allDBs2, source) {
		t.Errorf("source DB %s still exists after teardown", source)
	}
	for _, c := range clones {
		if has(allDBs2, c) {
			t.Errorf("clone DB %s still exists after teardown", c)
		}
	}
	if !has(allDBs2, tmpl) {
		t.Errorf("template DB %s was dropped (should survive teardown!)", tmpl)
	}
	t.Logf("after teardown: remaining DBs = %v", filter(allDBs2, "tm_td_", "_tm_"))
}

func listDatabases(t *testing.T) []string {
	t.Helper()
	dsn := "root:rootpw@tcp(127.0.0.1:13386)/"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SHOW DATABASES")
	if err != nil {
		t.Fatal(err)
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

// pickByPrefix returns the first entry starting with `prefix`.
func pickByPrefix(all []string, prefix, _ string) string {
	for _, s := range all {
		if strings.HasPrefix(s, prefix) {
			return s
		}
	}
	return ""
}

// splitSourceAndClones partitions DBs sharing a prefix into the
// source (no `_wN` suffix) and the clones (`_w<digit>+` suffix).
// Worktree slugs themselves start with `wt_`, so we need to look at
// the trailing token only.
func splitSourceAndClones(all []string, prefix string) (string, []string) {
	var source string
	var clones []string
	for _, s := range all {
		if !strings.HasPrefix(s, prefix) {
			continue
		}
		last := s[strings.LastIndex(s, "_")+1:]
		// Clones end with "w" + digits. The slug-based source ends
		// with a hex hash (no leading "w" alone in the final token).
		if len(last) >= 2 && last[0] == 'w' && allDigits(last[1:]) {
			clones = append(clones, s)
		} else {
			source = s
		}
	}
	return source, clones
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

func has(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

func filter(all []string, prefixes ...string) []string {
	var out []string
	for _, s := range all {
		for _, p := range prefixes {
			if strings.HasPrefix(s, p) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// unused-import suppressor for the `time` package when nothing else
// references it directly in this file's import list above.
var (
	_ = fmt.Sprintf
	_ = time.Second
)
