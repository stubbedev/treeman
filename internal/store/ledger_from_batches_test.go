package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func laravelName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// Regression for #214: a dump whose ledger applied a later-dated
// migration in an earlier batch than one treeman applied afterwards.
// Laravel unwinds the newer batch first, so editing the later-dated
// migration must unwind both.
func TestFindRollbackAncestor_DumpLedgerFromBatches(t *testing.T) {
	ctx := context.Background()
	s := openRepoStore(t)
	migs := "database/migrations/*.php"
	a := FileHash{Path: "database/migrations/2024_01_01_a.php", Hash: "ha"}
	b := FileHash{Path: "database/migrations/2024_02_01_b.php", Hash: "hb"}
	c := FileHash{Path: "database/migrations/2024_03_01_c.php", Hash: "hc"}
	inputs := map[string]InputVector{migs: {a, b, c}}

	ledger, err := LedgerFromBatches(inputs, nil, map[string]int{
		"2024_01_01_a": 1,
		"2024_03_01_c": 1,
		"2024_02_01_b": 2,
	}, laravelName)
	if err != nil {
		t.Fatal(err)
	}
	tpl := record("fpDump", "ch", inputs)
	tpl.Ledger = ledger
	if err := s.RecordSnapshot(ctx, tpl); err != nil {
		t.Fatal(err)
	}

	got, steps, err := findRollback(ctx, s, nil, map[string]InputVector{migs: {a, b, {Path: c.Path, Hash: "hcNEW"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected fpDump ancestor")
	}
	if steps != 2 {
		t.Errorf("steps = %d, want 2 (the dump applied c in batch 1, treeman applied b in batch 2)", steps)
	}
}

func TestLedgerFromBatches(t *testing.T) {
	migs := "database/migrations/*.php"
	locks := "composer.lock"
	inputs := map[string]InputVector{
		migs:  {{Path: "database/migrations/2024_02_01_b.php"}, {Path: "database/migrations/2024_03_01_c.php"}},
		locks: {{Path: "composer.lock"}},
	}
	lockfileGlobs := map[string]struct{}{locks: {}}
	cases := []struct {
		name    string
		batches map[string]int
		wantErr string
	}{
		{
			name:    "every input recorded, lockfile not needed",
			batches: map[string]int{"2024_02_01_b": 1, "2024_03_01_c": 2},
		},
		{
			name:    "squashed migration recorded before every input",
			batches: map[string]int{"2023_01_01_squashed": 1, "2024_02_01_b": 1, "2024_03_01_c": 2},
		},
		{
			name:    "input without a row",
			batches: map[string]int{"2024_02_01_b": 1},
			wantErr: `no row for input database/migrations/2024_03_01_c.php`,
		},
		{
			name:    "unknown migration a rollback would reach",
			batches: map[string]int{"2024_02_01_b": 1, "2024_03_01_c": 1, "2024_04_01_gone": 2},
			wantErr: `records "2024_04_01_gone" in batch 2`,
		},
		{
			name:    "unknown migration after the first input in its batch",
			batches: map[string]int{"2024_02_01_b": 1, "2024_02_15_gone": 1, "2024_03_01_c": 1},
			wantErr: `records "2024_02_15_gone" in batch 1`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger, err := LedgerFromBatches(inputs, lockfileGlobs, tc.batches, laravelName)
			if tc.wantErr == "" {
				if err != nil || ledger == nil {
					t.Fatalf("LedgerFromBatches = %v, %v; want a ledger", ledger, err)
				}
				for _, fh := range inputs[migs] {
					if got, want := ledger.batch[fh.Path], tc.batches[laravelName(fh.Path)]; got != want {
						t.Errorf("batch of %s = %d, want %d", fh.Path, got, want)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if ledger != nil {
				t.Errorf("ledger = %v, want none with the error", ledger)
			}
		})
	}
}
