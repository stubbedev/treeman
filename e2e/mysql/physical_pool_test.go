//go:build e2e

package mysql_e2e

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
)

func TestConcurrentPhysicalClonesFitInASingleConnectionPool(t *testing.T) {
	waitMySQL(t)
	t.Setenv("TREEMAN_MYSQL_PHYSICAL_MIN_BYTES", "0")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	drv, err := dbmysql.Connect(ctx, config.MysqlConn{
		Host: "127.0.0.1", Port: 3306, User: "root", Password: "rootpw",
		PoolMax:      1,
		ContainerRef: config.ContainerRef{Container: "treeman-e2e-mysql"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = drv.Close() }()

	const src = "tm_pool1_src"
	targets := []string{"tm_pool1_a", "tm_pool1_b"}
	t.Cleanup(func() {
		for _, n := range append([]string{src}, targets...) {
			_ = drv.DropDatabase(context.Background(), n)
		}
	})
	for _, q := range []string{
		"DROP DATABASE IF EXISTS " + src, "CREATE DATABASE " + src,
		"CREATE TABLE " + src + ".parent (id INT PRIMARY KEY)",
		"CREATE TABLE " + src + ".child (id INT PRIMARY KEY, parent_id INT, FOREIGN KEY (parent_id) REFERENCES " + src + ".parent(id))",
		"INSERT INTO " + src + ".parent VALUES (1), (2)",
		"INSERT INTO " + src + ".child VALUES (1, 1), (2, 2), (3, 2)",
	} {
		if _, err := drv.DB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(targets))
	for _, target := range targets {
		wg.Go(func() {
			if err := drv.SnapshotCreate(ctx, src, target); err != nil {
				errs <- fmt.Errorf("clone %s into %s: %w", src, target, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	if got := drv.LastCloneStrategy(); got != dbmysql.CloneStrategyPhysical {
		t.Errorf("clone strategy = %s, want %s", got, dbmysql.CloneStrategyPhysical)
	}
	for _, target := range targets {
		if got := rowCount(t, "127.0.0.1:13306", target, "child"); got != 3 {
			t.Errorf("%s.child rows = %d, want 3", target, got)
		}
	}
}
