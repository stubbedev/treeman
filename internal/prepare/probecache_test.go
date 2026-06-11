package prepare

import (
	"context"
	"errors"
	"testing"
)

// TestCachedProbeMemoizesPerRun guards the per-run probe cache: the
// second call under the same key must return the first result without
// re-invoking the probe.
func TestCachedProbeMemoizesPerRun(t *testing.T) {
	ctx := withProbeCache(context.Background())
	calls := 0
	probe := func() (string, error) { calls++; return "8.4.0", nil }
	for range 3 {
		v, err := cachedProbe(ctx, "mysql-version", probe)
		if err != nil || v != "8.4.0" {
			t.Fatalf("cachedProbe = %q, %v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("probe ran %d times, want 1", calls)
	}
	// Distinct key probes independently.
	if _, err := cachedProbe(ctx, "mysql-maxconns", func() (string, error) { calls++; return "x", nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("probe ran %d times after second key, want 2", calls)
	}
}

// TestCachedProbeErrorsNotCached: a failed probe must not poison the
// key — the next caller retries and can succeed.
func TestCachedProbeErrorsNotCached(t *testing.T) {
	ctx := withProbeCache(context.Background())
	calls := 0
	if _, err := cachedProbe(ctx, "k", func() (int, error) { calls++; return 0, errors.New("boom") }); err == nil {
		t.Fatal("want error from first probe")
	}
	v, err := cachedProbe(ctx, "k", func() (int, error) { calls++; return 42, nil })
	if err != nil || v != 42 {
		t.Fatalf("retry = %d, %v", v, err)
	}
	if calls != 2 {
		t.Fatalf("probe ran %d times, want 2", calls)
	}
}

// TestCachedProbeWithoutCacheFallsThrough: contexts without a cache
// (direct test/CLI paths) call the probe every time.
func TestCachedProbeWithoutCacheFallsThrough(t *testing.T) {
	calls := 0
	for range 2 {
		if _, err := cachedProbe(context.Background(), "k", func() (int, error) { calls++; return 1, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("probe ran %d times, want 2 (no cache in ctx)", calls)
	}
}
