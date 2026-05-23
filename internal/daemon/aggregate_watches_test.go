package daemon

import (
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

// TestAggregateWatchesTagsByDB asserts that top-level
// `watcher.paths` entries get DBIndex = -1 and per-DB entries get
// the database's index. Order is preserved (top-level first, then
// per-DB in declaration order).
func TestAggregateWatchesTagsByDB(t *testing.T) {
	cfg := &config.Config{
		Watcher: config.WatcherConfig{
			Paths: []config.WatcherPath{
				{Glob: "seeds/**/*.sql", On: "rebuild"},
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine: "mysql",
				Watch: []config.WatcherPath{
					{Glob: "db1/migrations/**/*.sql", On: "auto"},
					{Glob: "db1/fixtures/**/*.yaml", On: "rebuild"},
				},
			},
			{
				Engine: "postgres",
				Watch: []config.WatcherPath{
					{Glob: "db2/migrations/**/*.sql", On: "delta"},
				},
			},
		},
	}
	out := aggregateWatches(cfg)
	if len(out.Paths) != 4 {
		t.Fatalf("paths=%d, want 4", len(out.Paths))
	}
	cases := []struct {
		idx    int
		glob   string
		dbIdx  int
		on     string
	}{
		{0, "seeds/**/*.sql", -1, "rebuild"},
		{1, "db1/migrations/**/*.sql", 0, "auto"},
		{2, "db1/fixtures/**/*.yaml", 0, "rebuild"},
		{3, "db2/migrations/**/*.sql", 1, "delta"},
	}
	for _, c := range cases {
		got := out.Paths[c.idx]
		if got.Glob != c.glob {
			t.Errorf("paths[%d].glob = %q, want %q", c.idx, got.Glob, c.glob)
		}
		if got.DBIndex != c.dbIdx {
			t.Errorf("paths[%d].DBIndex = %d, want %d", c.idx, got.DBIndex, c.dbIdx)
		}
		if got.On != c.on {
			t.Errorf("paths[%d].On = %q, want %q", c.idx, got.On, c.on)
		}
	}
}
