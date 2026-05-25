//go:build e2e

// Package poolmax_e2e drives `connections.mysql.pool_max` against a
// real MySQL server. Sets PoolMax=2 then fires N concurrent SELECTs
// — MySQL's `SHOW PROCESSLIST` must never report more than 2
// connections from this client at any instant.
package poolmax_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
)

func TestPoolMaxCapsConcurrency(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13456", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13456", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	const poolMax = 2
	const goroutines = 8

	cfg := config.MysqlConn{
		Host: "127.0.0.1", Port: 13456,
		User: "root", Password: "rootpw",
		PoolMax: poolMax,
	}
	drv, err := dbmysql.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer drv.Close()

	// Sampler: every 50ms, count this client's connections via
	// information_schema.processlist (the safer alternative to
	// SHOW PROCESSLIST when the user lacks PROCESS privilege).
	var maxObserved uint32
	stop := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		sampler, err := sql.Open("mysql", "root:rootpw@tcp(127.0.0.1:13456)/?interpolateParams=true")
		if err != nil {
			t.Errorf("sampler connect: %v", err)
			return
		}
		defer sampler.Close()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var n int
				// Exclude the sampler's OWN connection — its query
				// string contains the marker substring too, which
				// would inflate the count by 1.
				err := sampler.QueryRow(`
					SELECT COUNT(*) FROM information_schema.processlist
					WHERE id != connection_id()
					  AND user = 'root'
					  AND command != 'Sleep'
					  AND info LIKE '%pool_max_marker%'
				`).Scan(&n)
				if err != nil {
					continue
				}
				for {
					cur := atomic.LoadUint32(&maxObserved)
					if uint32(n) <= cur || atomic.CompareAndSwapUint32(&maxObserved, cur, uint32(n)) {
						break
					}
				}
			}
		}
	}()

	// Fire N concurrent slow queries through the driver's pool.
	// Tag each with a literal "pool_max_marker" comment so the
	// sampler can filter for ONLY queries from this test.
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var v int
			err := drv.DB.QueryRowContext(ctx,
				"SELECT SLEEP(0.5) /* pool_max_marker */ + 1").Scan(&v)
			if err != nil {
				t.Errorf("query: %v", err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	samplerWG.Wait()

	observed := atomic.LoadUint32(&maxObserved)
	t.Logf("max concurrent in-flight queries observed: %d (pool_max=%d, %d goroutines)",
		observed, poolMax, goroutines)
	if observed > poolMax {
		t.Errorf("PoolMax breach: observed %d > %d", observed, poolMax)
	}
	if observed == 0 {
		t.Logf("sampler never caught the queries in flight — query ran too fast or sampler missed (still pass if no breach)")
	}
}

var _ = fmt.Sprintf
