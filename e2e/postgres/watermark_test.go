//go:build e2e

package postgres_e2e

import (
	"context"
	"database/sql"
	"net"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	dbpostgres "github.com/stubbedev/treeman/internal/db/postgres"
)

// TestWriteWatermarkSeesStatsReset: pg_stat_reset() zeroes the row
// counters, so a database written to and then reset used to read the
// token it had before the writes (#232).
func TestWriteWatermarkSeesStatsReset(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", time.Second)
		if err != nil {
			return err
		}
		return c.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	drv, err := dbpostgres.Connect(ctx, *buildConfig().Connections.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	const db = "tm_watermark"
	mustExecPG(t, "DROP DATABASE IF EXISTS "+db)
	mustExecPG(t, "CREATE DATABASE "+db)
	t.Cleanup(func() { mustExecPG(t, "DROP DATABASE IF EXISTS "+db) })

	start, err := drv.WriteWatermark(ctx, db)
	if err != nil || start == "" {
		t.Fatalf("watermark of %s = %q, err %v", db, start, err)
	}

	conn, err := sql.Open("pgx", "postgres://postgres:pgpw@127.0.0.1:15432/"+db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, q := range []string{
		"CREATE TABLE a (id INT)",
		"INSERT INTO a VALUES (1), (2)",
		"SELECT pg_stat_force_next_flush()",
		"SELECT pg_stat_reset()",
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if w, err := drv.WriteWatermark(ctx, db); err != nil || w == start {
		t.Fatalf("after writes and pg_stat_reset(): watermark = %q (err %v), want it to differ from %q", w, err, start)
	}
}
