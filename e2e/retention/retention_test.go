//go:build e2e

// Package retention_e2e exercises snapshot retention: with
// CapPerRepo set low, building more snapshots than the cap should
// trigger LRU eviction. The oldest template DB must be dropped from
// the engine when the cap is exceeded.
package retention_e2e

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
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

func TestCapPerRepoEvictsOldSnapshots(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13436", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13436", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Five distinct worktrees → five different fingerprints. Cap
	// is 3, so after the 5th run, only 3 most-recent templates
	// should remain in MySQL.
	const cap = 3
	const N = 5
	templates := []string{}
	repoID, err := st.EnsureRepo(ctx, "/tmp/ret-repo", "ret-repo")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		wt := t.TempDir()
		if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
			[]byte(fmt.Sprintf("CREATE TABLE w%d (id INT);", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{
			Connections: config.ConnectionsConfig{
				Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13436, User: "root", Password: "rootpw"},
			},
			Snapshots: config.SnapshotsConfig{CapPerRepo: cap},
			Databases: []config.DatabaseConfig{
				{
					Engine:       "mysql",
					NameTemplate: fmt.Sprintf("tm_ret_%d_{slug}", i),
					Dump:         config.DumpList{{Path: "seed.sql"}},
				},
			},
		}
		sl := slug.For(wt, fmt.Sprintf("branch-%d", i))
		wtID, err := st.EnsureWorktree(ctx, repoID, wt, sl.Value, fmt.Sprintf("branch-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		outs, err := prepare.Run(ctx, cfg, wt, sl, st, repoID, wtID, nil)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		o := outs[0]
		templates = append(templates, o.TemplateName)
		t.Logf("run %d: template=%s", i, o.TemplateName)
		time.Sleep(100 * time.Millisecond) // ensure distinct last_used_at
	}

	// EvictExcess runs in a goroutine after each RecordSnapshot;
	// poll for the template count to settle at `cap`.
	harness.WaitForReady(t, "lru-eviction-settle", 10*time.Second, func() error {
		dbs := listDatabases(t)
		var alive []string
		for _, d := range dbs {
			if strings.HasPrefix(d, "_tm_") {
				alive = append(alive, d)
			}
		}
		if len(alive) != cap {
			return fmt.Errorf("template count=%d want %d (have: %v)", len(alive), cap, alive)
		}
		return nil
	})

	dbs := listDatabases(t)
	var alive []string
	for _, d := range dbs {
		if strings.HasPrefix(d, "_tm_") {
			alive = append(alive, d)
		}
	}
	t.Logf("templates alive after %d runs (cap=%d): %v", N, cap, alive)

	// The 2 oldest (templates[0], templates[1]) should be evicted;
	// the 3 newest should survive.
	survived := map[string]bool{}
	for _, a := range alive {
		survived[a] = true
	}
	for i := 0; i < N-cap; i++ {
		if survived[templates[i]] {
			t.Errorf("template %d (%s) was NOT evicted but should be (LRU)", i, templates[i])
		}
	}
	for i := N - cap; i < N; i++ {
		if !survived[templates[i]] {
			t.Errorf("template %d (%s) was evicted but should survive (MRU)", i, templates[i])
		}
	}
}

func listDatabases(t *testing.T) []string {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp(127.0.0.1:13436)/")
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
