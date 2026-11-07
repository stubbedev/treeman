//go:build e2e

package mysql_e2e

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
	"github.com/stubbedev/treeman/internal/db/restorestage"
)

func TestRestoreKeepsTargetExistingWhileAClientRecreatesIt(t *testing.T) {
	waitMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	drv, err := dbmysql.Connect(ctx, *buildConfig().Connections.Mysql)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drv.Close() }()

	const src, tpl, target = "tm_swap_src", "tm_swap_tpl", "tm_swap_target"
	t.Cleanup(func() {
		for _, n := range []string{src, tpl, target} {
			_ = drv.DropDatabase(context.Background(), n)
		}
	})
	for _, q := range []string{
		"DROP DATABASE IF EXISTS " + src, "CREATE DATABASE " + src,
		"CREATE TABLE " + src + ".parent (id INT PRIMARY KEY)",
		"CREATE TABLE " + src + ".child (id INT PRIMARY KEY, parent_id INT, FOREIGN KEY (parent_id) REFERENCES " + src + ".parent(id))",
		"CREATE TABLE " + src + ".audit (n INT)",
		"INSERT INTO " + src + ".parent VALUES (1)", "INSERT INTO " + src + ".child VALUES (1, 1)",
		"CREATE VIEW " + src + ".v AS SELECT id FROM " + src + ".child",
		"CREATE TRIGGER " + src + ".child_ai AFTER INSERT ON " + src + ".child FOR EACH ROW INSERT INTO audit VALUES (NEW.id)",
	} {
		if _, err := drv.DB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := drv.SnapshotCreate(ctx, src, tpl); err != nil {
		t.Fatalf("snapshot create: %v", err)
	}
	if err := drv.SnapshotRestore(ctx, tpl, target); err != nil {
		t.Fatalf("first restore: %v", err)
	}

	var vanished atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ok, err := drv.DatabaseExists(ctx, target)
			if err == nil && !ok {
				vanished.Add(1)
				_, _ = drv.DB.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+target)
			}
		}
	}()
	for i := range 10 {
		if err := drv.SnapshotRestoreStaged(ctx, tpl, target); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("restore %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if n := vanished.Load(); n > 0 {
		t.Fatalf("%s was observed missing %d times during restores", target, n)
	}

	db := openMySQL(t, "127.0.0.1:13306", target)
	defer db.Close()
	if _, err := db.ExecContext(ctx, "INSERT INTO child VALUES (2, 1)"); err != nil {
		t.Fatalf("insert through the restored FK: %v", err)
	}
	var audited, viewRows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit").Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("trigger in the restored target: audit rows=%d err=%v", audited, err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM v").Scan(&viewRows); err != nil || viewRows != 2 {
		t.Fatalf("view in the restored target: rows=%d err=%v", viewRows, err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO child VALUES (3, 99)"); err == nil {
		t.Fatal("restored FK does not point at the target's parent table")
	}
	var refSchema string
	if err := db.QueryRowContext(ctx,
		`SELECT REFERENCED_TABLE_SCHEMA FROM information_schema.KEY_COLUMN_USAGE
		 WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'child' AND REFERENCED_TABLE_NAME IS NOT NULL`, target).Scan(&refSchema); err != nil || refSchema != target {
		t.Fatalf("child FK references schema %q (err %v), want %q", refSchema, err, target)
	}

	left, err := drv.ListMatching(ctx, restorestage.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Fatalf("restore stages left behind: %v", left)
	}
}

func TestRestoreReapsStagesOfACrashedRun(t *testing.T) {
	waitMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	drv, err := dbmysql.Connect(ctx, *buildConfig().Connections.Mysql)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drv.Close() }()

	crashed := fmt.Sprintf("_tmrs_%x_0123456789abcdef_n", time.Now().Add(-48*time.Hour).Unix())
	live := fmt.Sprintf("_tmrs_%x_fedcba9876543210_n", time.Now().Unix())
	for _, n := range []string{crashed, live, "tm_reap_tpl"} {
		if err := drv.EnsureDB(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{crashed, live, "tm_reap_tpl", "tm_reap_target"} {
			_ = drv.DropDatabase(context.Background(), n)
		}
	})
	if err := drv.SnapshotRestore(ctx, "tm_reap_tpl", "tm_reap_target"); err != nil {
		t.Fatal(err)
	}
	if ok, err := drv.DatabaseExists(ctx, crashed); err != nil || ok {
		t.Fatalf("crashed run's stage %s survived (err %v)", crashed, err)
	}
	if ok, err := drv.DatabaseExists(ctx, live); err != nil || !ok {
		t.Fatalf("live stage %s was reaped (err %v)", live, err)
	}
}
