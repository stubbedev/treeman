package prepare

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeRowNS is an nsDriver whose Capture can be told to "lose" rows,
// for pinning the #41 verification invariants.
type fakeRowNS struct {
	capture func(active, durable string) error

	names  map[string][]string         // ns → table names (sorted sample order)
	counts map[string]map[string]int64 // ns → table → rows
	drops  []string
}

func (f *fakeRowNS) Exists(ctx context.Context, ns string) (bool, error) { return true, nil }
func (f *fakeRowNS) Capture(ctx context.Context, active, durable string) error {
	if f.capture != nil {
		return f.capture(active, durable)
	}
	return nil
}
func (f *fakeRowNS) Restore(ctx context.Context, durable, active string) error { return nil }
func (f *fakeRowNS) RestoreParent(ctx context.Context, p, a string, k func(string) bool) error {
	return nil
}
func (f *fakeRowNS) Empty(ctx context.Context, active string) error { return nil }
func (f *fakeRowNS) Drop(ctx context.Context, ns string) error      { return nil }
func (f *fakeRowNS) DropDurable(ctx context.Context, durable string) error {
	f.drops = append(f.drops, durable)
	return nil
}

func (f *fakeRowNS) Watermark(ctx context.Context, ns string) (string, error) {
	return "", nil
}

func (f *fakeRowNS) RowCountSample(ctx context.Context, ns string) (map[string]int64, error) {
	tables, ok := f.names[ns]
	if !ok {
		return nil, fmt.Errorf("no such namespace %s", ns)
	}
	out := map[string]int64{}
	for _, t := range tables {
		if n := f.counts[ns][t]; n > 0 {
			out[t] = n
		}
	}
	return out, nil
}

func (f *fakeRowNS) RowCountsFor(ctx context.Context, ns string, tables []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range tables {
		out[t] = f.counts[ns][t] // missing table → 0, by map semantics
	}
	return out, nil
}

// TestCaptureVerifiedDropsEmptyCopy: a copy that comes back schema-only
// while the source had rows is DROPPED and reported, never recorded
// (the #41 corruption path).
func TestCaptureVerifiedDropsEmptyCopy(t *testing.T) {
	ns := &fakeRowNS{
		names: map[string][]string{"src": {"users", "clients"}, "dur": {"users", "clients"}},
		counts: map[string]map[string]int64{
			"src": {"users": 10, "clients": 42},
			"dur": {"users": 0, "clients": 0}, // schema-only copy
		},
	}
	err := captureVerified(context.Background(), ns, "src", "dur")
	if err == nil {
		t.Fatal("schema-only copy must fail verification")
	}
	if len(ns.drops) != 1 || ns.drops[0] != "dur" {
		t.Fatalf("bad copy must be dropped, got %v", ns.drops)
	}
}

// TestCaptureVerifiedDropsPartialCopy: a copy missing SOME tables'
// rows (the 8-of-243 case) is caught by the exact-count sample.
func TestCaptureVerifiedDropsPartialCopy(t *testing.T) {
	ns := &fakeRowNS{
		names: map[string][]string{"src": {"a", "b", "c"}, "dur": {"a", "b", "c"}},
		counts: map[string]map[string]int64{
			"src": {"a": 1, "b": 2, "c": 3},
			"dur": {"a": 1, "b": 0, "c": 3}, // b lost its rows
		},
	}
	if err := captureVerified(context.Background(), ns, "src", "dur"); err == nil {
		t.Fatal("partial copy must fail verification")
	}
	if len(ns.drops) != 1 {
		t.Fatalf("bad copy must be dropped, got %v", ns.drops)
	}
}

// TestCaptureVerifiedPassesHealthyCopy: equal-or-higher counts pass.
func TestCaptureVerifiedPassesHealthyCopy(t *testing.T) {
	ns := &fakeRowNS{
		names: map[string][]string{"src": {"a"}, "dur": {"a"}},
		counts: map[string]map[string]int64{
			"src": {"a": 5},
			"dur": {"a": 7}, // source kept growing during the copy
		},
	}
	if err := captureVerified(context.Background(), ns, "src", "dur"); err != nil {
		t.Fatalf("healthy copy must pass: %v", err)
	}
	if len(ns.drops) != 0 {
		t.Fatalf("healthy copy must not be dropped, got %v", ns.drops)
	}
}

// TestCaptureVerifiedEmptySourcePasses: a legitimately empty source
// (no rows anywhere) captures an empty copy without complaint.
func TestCaptureVerifiedEmptySourcePasses(t *testing.T) {
	ns := &fakeRowNS{
		names: map[string][]string{"src": {}, "dur": {}},
		counts: map[string]map[string]int64{
			"src": {}, "dur": {},
		},
	}
	if err := captureVerified(context.Background(), ns, "src", "dur"); err != nil {
		t.Fatalf("empty source must pass: %v", err)
	}
}

// TestCaptureVerifiedProbeFailureIsNotFatal: when the row-count probe
// itself errors (engine without stats, transient catalog hiccup),
// verification is skipped — a successful capture stands.
func TestCaptureVerifiedProbeFailureIsNotFatal(t *testing.T) {
	ns := &fakeRowNS{counts: map[string]map[string]int64{}} // names missing → probe errors
	if err := captureVerified(context.Background(), ns, "src", "dur"); err != nil {
		t.Fatalf("probe failure must not fail the capture: %v", err)
	}
}

// TestCaptureVerifiedCaptureErrorPropagates: the underlying capture
// error surfaces unchanged (after retries).
func TestCaptureVerifiedCaptureErrorPropagates(t *testing.T) {
	boom := errors.New("capture exploded")
	ns := &fakeRowNS{capture: func(string, string) error { return boom }}
	if err := captureVerified(context.Background(), ns, "src", "dur"); !errors.Is(err, boom) {
		t.Fatalf("capture error must propagate, got %v", err)
	}
}
