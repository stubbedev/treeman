//go:build e2e

// Package recover_e2e verifies the failed-worktree recovery contract:
// when a finalize FAILS after engine prepare has already built the
// per-worktree DB, the daemon resets the primary namespace so the
// documented retry (`treeman wt finalize`) cold-rebuilds end-to-end
// instead of tripping on half-applied / already-existing state.
//
// Without that recovery wired into the finalize failure path, the
// source DB would survive a failed finalize and a retry would have to
// reconcile against it. The two assertions below pin the guarantee:
//
//  1. After a finalize whose create-after-engines hook fails, the
//     source DB is GONE (recovery dropped it) and the row derives a
//     `worktree:create:error` plus a `worktree:recover:drop` event.
//  2. With the failing hook removed, a second finalize succeeds and the
//     source DB is rebuilt — proving recovery left a clean slate.
package recover_e2e

import (
	"context"
	"database/sql"
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

const dbPrefix = "tm_rec_"

func TestFinalizeFailureRecoversNamespace(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13466", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13466", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "seed.sql"),
		"CREATE TABLE t (id INT); INSERT INTO t VALUES (1),(2),(3);")
	writeFile(t, filepath.Join(repoRoot, ".env"), "MYSQL_PW=rootpw\n")
	writeFile(t, filepath.Join(repoRoot, ".git/HEAD"), "ref: refs/heads/main\n")

	// Base config minus the hooks block — phase 2 reuses it verbatim.
	base := `
worktrees:
  root: .worktrees
env_sources: [.env]
connections:
  mysql:
    host: 127.0.0.1
    port: 13466
    user: root
    password: $MYSQL_PW
databases:
  - engine: mysql
    name_template: tm_rec_{slug}
    dump: seed.sql
`
	// Phase 1: a create-after-engines hook that always fails. Prepare
	// has already built the source DB by the time it fires, so its
	// failure exercises exactly the "DB exists, then finalize errors"
	// path that recovery must clean up.
	writeFile(t, filepath.Join(repoRoot, ".treeman.yaml"), base+`
hooks:
  create-after-engines:
    - run: "false"
`)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	state := daemon.NewState(ctx, st)
	env := map[string]string{"PATH": os.Getenv("PATH"), "MYSQL_PW": "rootpw"}

	// ── 1. Finalize fails in the after-engines hook ──
	// FinalizeWorktree records the failure as a terminal event and then
	// deliberately returns nil (so dispatch doesn't double-log), so the
	// failure is asserted via the event log below, not the return value.
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env); err != nil {
		t.Fatalf("finalize returned a hard error: %v", err)
	}

	// The source DB built by prepare must have been dropped by recovery.
	if dbs := listMatching(t, dbPrefix); len(dbs) != 0 {
		t.Fatalf("source DB survived a failed finalize (recovery did not run): %v", dbs)
	}

	// The row must derive `error`, and a recovery-drop event must prove
	// the namespace reset actually happened.
	wtID := lookupWorktreeID(t, st, repoRoot)
	assertHasEvent(t, st, wtID, store.EvtWorktreeCreateError)
	assertHasEvent(t, st, wtID, store.EvtWorktreeRecoverDrop)

	// ── 2. Drop the failing hook, retry → clean cold rebuild ──
	writeFile(t, filepath.Join(repoRoot, ".treeman.yaml"), base)
	if err := daemon.FinalizeWorktree(ctx, state, repoRoot, repoRoot, env); err != nil {
		t.Fatalf("retry finalize after recovery: %v", err)
	}
	if dbs := listMatching(t, dbPrefix); len(dbs) == 0 {
		t.Fatal("source DB not rebuilt on retry — recovery did not leave a clean slate")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listMatching(t *testing.T, prefix string) []string {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp(127.0.0.1:13466)/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SHOW DATABASES")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func lookupWorktreeID(t *testing.T, st *store.Store, path string) int64 {
	t.Helper()
	row, err := st.LookupActiveWorktreeByPath(context.Background(), path)
	if err != nil {
		t.Fatalf("lookup worktree row: %v", err)
	}
	if row.ID == 0 {
		t.Fatal("worktree row not created")
	}
	return row.ID
}

func assertHasEvent(t *testing.T, st *store.Store, wtID int64, evType string) {
	t.Helper()
	evs, err := st.QueryEvents(context.Background(), store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{evType},
		Limit:      1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Errorf("expected a %q event for worktree %d, found none", evType, wtID)
	}
}
