//go:build e2e

package mysql_e2e

import (
	"context"
	"testing"
	"time"

	dbmysql "github.com/stubbedev/treeman/internal/db/mysql"
)

// TestWriteWatermarkSeesWritesHiddenByDDL: performance_schema drops a
// table's COUNT_WRITE back to 0 on DDL, so rows written and then
// followed by ADD INDEX used to leave the token where it started, and
// schema-only DDL never moved it (#232).
func TestWriteWatermarkSeesWritesHiddenByDDL(t *testing.T) {
	waitMySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	drv, err := dbmysql.Connect(ctx, *buildConfig().Connections.Mysql)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	const db = "tm_watermark"
	t.Cleanup(func() { _ = drv.DropDatabase(context.Background(), db) })
	exec := func(q string) {
		t.Helper()
		if _, err := drv.DB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	watermark := func() string {
		t.Helper()
		w, err := drv.WriteWatermark(ctx, db)
		if err != nil || w == "" {
			t.Fatalf("watermark of %s = %q, err %v", db, w, err)
		}
		return w
	}
	exec("DROP DATABASE IF EXISTS " + db)
	exec("CREATE DATABASE " + db)
	exec("CREATE TABLE " + db + ".a (id INT PRIMARY KEY, v INT)")

	start := watermark()
	if again := watermark(); again != start {
		t.Fatalf("untouched database: watermark moved from %q to %q", start, again)
	}
	exec("INSERT INTO " + db + ".a VALUES (1, 1)")
	exec("CREATE INDEX iv ON " + db + ".a (v)")
	afterIndex := watermark()
	if afterIndex == start {
		t.Fatalf("a row write followed by ADD INDEX left the watermark at %q", start)
	}
	exec("CREATE VIEW " + db + ".vv AS SELECT id FROM " + db + ".a")
	if w := watermark(); w == afterIndex {
		t.Fatalf("CREATE VIEW left the watermark at %q", w)
	}
}
