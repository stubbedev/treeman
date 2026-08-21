//go:build e2e

package extras_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
)

func openDB(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root:rootpw@tcp(127.0.0.1:13416)/%s", dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func hasTable(t *testing.T, dbName, table string) bool {
	t.Helper()
	db := openDB(t, dbName)
	defer db.Close()
	var n int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ?",
		dbName, table,
	).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func hasDatabase(t *testing.T, name string) bool {
	t.Helper()
	for _, d := range listDatabases(t) {
		if d == name {
			return true
		}
	}
	return false
}

func listDatabases(t *testing.T) []string {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp(127.0.0.1:13416)/")
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

// prepareErr runs prepare without harness's t.Fatalf-on-error so the
// caller can assert on the error string.
func prepareErr(env *harness.Env, cfg *config.Config) ([]prepare.Outcome, error) {
	return prepare.Run(env.Ctx, cfg, env.WTPath, env.Slug, env.Store,
		env.RepoID, env.WTID, nil)
}

// runFinalize stands up a real daemon State + FinalizeWorktree
// against the given repo dir. Used by tests that need full
// daemon-mediated execution (hook actions, etc.).
func runFinalize(t *testing.T, repoRoot string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	state := daemon.NewState(ctx, st)
	env := map[string]string{
		"PATH":     os.Getenv("PATH"),
		"MYSQL_PW": "rootpw",
	}
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env, false); err != nil {
		t.Fatalf("FinalizeWorktree: %v", err)
	}
	// Stash for startWatcher.
	tStates[t.Name()] = state
}

// startWatcher launches the per-worktree watchers (HEAD + FS) so
// file-change hooks fire. Must be called after runFinalize.
func startWatcher(t *testing.T, repoRoot string) {
	t.Helper()
	state := tStates[t.Name()]
	if state == nil {
		t.Fatal("startWatcher needs runFinalize first")
	}
	if err := daemon.ResumeWorktreeWatcher(context.Background(), state, repoRoot, repoRoot); err != nil {
		t.Fatalf("ResumeWorktreeWatcher: %v", err)
	}
}

// tStates carries the daemon.State across helper calls within a
// single test. Keyed by t.Name() so subtests don't clobber each
// other.
var tStates = map[string]*daemon.State{}
