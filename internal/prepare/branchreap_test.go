package prepare

import "testing"

// TestESDurablePrefix pins the orphan-reconcile classifier: it must recognise a
// treeman ES branch durable ("tmbs_<16hex>_…") and its family prefix, and must
// reject every other ES index family the reconcile must never touch — snapshot
// cache (`tm_`), active clones (`kho_`), and base data (`client_*`/`dev_*`). A
// false positive here would drop live or cached data.
func TestESDurablePrefix(t *testing.T) {
	cases := []struct {
		name       string
		index      string
		wantPrefix string
		wantOK     bool
	}{
		{"durable with appended index", "tmbs_eef6294a51701034__client_48_category_232_pim_end", "tmbs_eef6294a51701034_", true},
		{"durable bare prefix", "tmbs_1a7984f692fa4bab_", "tmbs_1a7984f692fa4bab_", true},
		{"snapshot cache template rejected", "tm_eef6294a51701034_client_48", "", false},
		{"active clone rejected", "kho_kon_12660_client_44_category_210_pim_end", "", false},
		{"base files alias rejected", "client_411_files_alias", "", false},
		{"base dev index rejected", "dev_client_48_category_232_pim_tree", "", false},
		{"hash too short rejected", "tmbs_abc_client", "", false},
		{"non-hex hash rejected", "tmbs_zzzzzzzzzzzzzzzz_client", "", false},
		{"missing trailing underscore rejected", "tmbs_eef6294a51701034client", "", false},
		{"empty rejected", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := esDurablePrefix(tc.index)
			if ok != tc.wantOK || got != tc.wantPrefix {
				t.Fatalf("esDurablePrefix(%q) = (%q, %t), want (%q, %t)",
					tc.index, got, ok, tc.wantPrefix, tc.wantOK)
			}
		})
	}
}
