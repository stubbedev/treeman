package prepare

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/store"
)

func ledgerBatches(t *testing.T, l *store.AppliedLedger) map[string]int {
	t.Helper()
	if l == nil {
		return nil
	}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]int
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestBuiltLedgerReadsDumpBatches: a template built on a restored dump
// takes its applied ledger from the framework's batch table (#214);
// without a dump, or when the dump has no batch table, every input
// counts as applied in one run.
func TestBuiltLedgerReadsDumpBatches(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	a := "database/migrations/2024_01_01_a.php"
	b := "database/migrations/2024_02_01_b.php"
	inputs := map[string]store.InputVector{"database/migrations/*.php": {{Path: a}, {Path: b}}}
	withDump := config.DatabaseConfig{Engine: "mysql", Dump: config.DumpList{{Path: "dump.sql"}}}
	laravelTable := func(ctx context.Context, db string, tb framework.BatchTable) (map[string]int, bool, error) {
		if db != "src" {
			t.Errorf("read batches of %q, want the source database", db)
		}
		if tb.Table != "migrations" {
			return nil, false, nil
		}
		return map[string]int{"2024_01_01_a": 3, "2024_02_01_b": 1}, true, nil
	}
	noTable := func(context.Context, string, framework.BatchTable) (map[string]int, bool, error) {
		return nil, false, nil
	}

	cases := []struct {
		name string
		d    config.DatabaseConfig
		read batchReader
		want map[string]int
	}{
		{name: "no dump", d: config.DatabaseConfig{Engine: "mysql"}, read: laravelTable, want: map[string]int{a: 0, b: 0}},
		{name: "engine without batch tables", d: withDump, read: nil, want: map[string]int{a: 0, b: 0}},
		{name: "dump without a batch table", d: withDump, read: noTable, want: map[string]int{a: 0, b: 0}},
		{name: "dump with a batch table", d: withDump, read: laravelTable, want: map[string]int{a: 3, b: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, err := builtLedger(ctx, st, 0, 0, tc.d, "src", inputs, tc.read)
			if err != nil {
				t.Fatal(err)
			}
			got := ledgerBatches(t, l)
			if len(got) != len(tc.want) {
				t.Fatalf("ledger = %v, want %v", got, tc.want)
			}
			for p, w := range tc.want {
				if got[p] != w {
					t.Errorf("batch of %s = %d, want %d", p, got[p], w)
				}
			}
		})
	}

	t.Run("batch table the inputs do not account for", func(t *testing.T) {
		partial := func(context.Context, string, framework.BatchTable) (map[string]int, bool, error) {
			return map[string]int{"2024_01_01_a": 1}, true, nil
		}
		l, err := builtLedger(ctx, st, 0, 0, withDump, "src", inputs, partial)
		if err != nil {
			t.Fatal(err)
		}
		if l != nil {
			t.Errorf("ledger = %v, want unknown", ledgerBatches(t, l))
		}
	})

	t.Run("read failure", func(t *testing.T) {
		boom := errors.New("boom")
		failing := func(context.Context, string, framework.BatchTable) (map[string]int, bool, error) {
			return nil, false, boom
		}
		if _, err := builtLedger(ctx, st, 0, 0, withDump, "src", inputs, failing); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want it to wrap the read failure", err)
		}
	})
}
