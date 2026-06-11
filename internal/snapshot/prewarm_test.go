package snapshot

import "testing"

// TestSpareNameSlotRoundTrip guards the naming contract shared by the
// prepare-side claim/replenish loops and the GC's spare-family reap.
func TestSpareNameSlotRoundTrip(t *testing.T) {
	const tpl = "tm_abc123"
	for _, slot := range []int{1, 2, 16} {
		name := SpareName(tpl, slot)
		got, ok := SpareSlot(name, tpl)
		if !ok || got != slot {
			t.Errorf("SpareSlot(SpareName(%d)) = %d, %v", slot, got, ok)
		}
	}
}

func TestSpareSlotRejectsNonSpares(t *testing.T) {
	const tpl = "tm_abc123"
	for _, name := range []string{
		tpl,                   // the template itself
		tpl + "_spare",        // missing slot index
		tpl + "_sparex",       // non-numeric slot
		tpl + "_spare0",       // slots are 1-based
		tpl + "_spare-1",      // negative
		"other" + "_spare1",   // different template
		tpl + "_w1",           // test-clone fanout name
		SpareName("other", 1), // spare of another template
	} {
		if slot, ok := SpareSlot(name, tpl); ok {
			t.Errorf("SpareSlot(%q) = %d, true; want false", name, slot)
		}
	}
}
