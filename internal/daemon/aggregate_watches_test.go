package daemon

import (
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

// TestAggregateWatchesTagsByDB asserts every input glob is projected
// into a WatcherPath tagged with the owning DBIndex and its label.
func TestAggregateWatchesTagsByDB(t *testing.T) {
	cfg := &config.Config{
		Databases: []config.DatabaseConfig{
			{
				Engine: "mysql",
				Inputs: []config.Input{
					{Glob: "db1/migrations/**/*.sql", Label: "migrations"},
					{Glob: "db1/fixtures/**/*.yaml", Label: "fixtures"},
				},
			},
			{
				Engine: "postgres",
				Inputs: []config.Input{
					{Glob: "db2/migrations/**/*.sql"}, // no label
				},
			},
		},
	}
	out := aggregateWatches(cfg)
	if len(out) != 3 {
		t.Fatalf("paths=%d, want 3", len(out))
	}
	cases := []struct {
		idx   int
		glob  string
		dbIdx int
		label string
	}{
		{0, "db1/migrations/**/*.sql", 0, "migrations"},
		{1, "db1/fixtures/**/*.yaml", 0, "fixtures"},
		{2, "db2/migrations/**/*.sql", 1, ""},
	}
	for _, c := range cases {
		got := out[c.idx]
		if got.Glob != c.glob {
			t.Errorf("paths[%d].glob = %q, want %q", c.idx, got.Glob, c.glob)
		}
		if got.DBIndex != c.dbIdx {
			t.Errorf("paths[%d].DBIndex = %d, want %d", c.idx, got.DBIndex, c.dbIdx)
		}
		if got.Label != c.label {
			t.Errorf("paths[%d].Label = %q, want %q", c.idx, got.Label, c.label)
		}
	}
}
