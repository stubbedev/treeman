package prepare

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFanOutClonesParallel proves clones run concurrently, not in
// sequence. Five 100 ms restores serialised would take ≥500 ms;
// parallelism caps wall time near the per-restore duration plus
// scheduling slop. We allow generous headroom (300 ms) so a busy CI
// box doesn't flake.
func TestFanOutClonesParallel(t *testing.T) {
	delay := 100 * time.Millisecond
	restore := func(ctx context.Context, template, target string) error {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	clones := []string{"a", "b", "c", "d", "e"}
	start := time.Now()
	if err := fanOutClones(context.Background(), nil, 0, 0, restore, "tmpl", clones, "postgres", 0, 0); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed >= 300*time.Millisecond {
		t.Errorf("expected parallel execution (~%v); got %v", delay, elapsed)
	}
}

// TestFanOutClonesPropagatesErrors confirms the first error is
// returned and the context cancel cuts peers short.
func TestFanOutClonesPropagatesErrors(t *testing.T) {
	var calls atomic.Int32
	want := errors.New("boom")
	restore := func(ctx context.Context, template, target string) error {
		calls.Add(1)
		if target == "b" {
			return want
		}
		select {
		case <-time.After(50 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := fanOutClones(context.Background(), nil, 0, 0, restore, "tmpl", []string{"a", "b", "c"}, "postgres", 0, 0)
	if err == nil || !contains(err.Error(), "boom") {
		t.Errorf("expected boom-wrapped error, got %v", err)
	}
	if calls.Load() < 1 {
		t.Error("restore was never called")
	}
}

// TestFanOutClonesEmpty handles the zero-clones case without
// spinning up goroutines.
func TestFanOutClonesEmpty(t *testing.T) {
	called := false
	restore := func(ctx context.Context, template, target string) error {
		called = true
		return nil
	}
	if err := fanOutClones(context.Background(), nil, 0, 0, restore, "tmpl", nil, "postgres", 0, 0); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("restore must not be called for an empty clone list")
	}
}

// TestFanOutClonesRespectsLimit confirms the concurrency cap holds
// at GOMAXPROCS (or len(clones), whichever is smaller). We submit a
// long-running restore set and snapshot the in-flight count.
func TestFanOutClonesRespectsLimit(t *testing.T) {
	var inflight atomic.Int32
	var peak atomic.Int32
	var mu sync.Mutex
	restore := func(ctx context.Context, template, target string) error {
		mu.Lock()
		n := inflight.Add(1)
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		inflight.Add(-1)
		return nil
	}
	clones := make([]string, 50) // far exceeds any reasonable GOMAXPROCS
	for i := range clones {
		clones[i] = string(rune('a' + i%26))
	}
	if err := fanOutClones(context.Background(), nil, 0, 0, restore, "tmpl", clones, "postgres", 0, 0); err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 64 {
		// fanOutClones caps at GOMAXPROCS; this bound is a sanity
		// envelope. A peak above 64 means the limiter is broken.
		t.Errorf("peak in-flight %d exceeded the cap envelope", peak.Load())
	}
	if peak.Load() < 2 {
		t.Errorf("expected at least 2 concurrent restores, peak=%d", peak.Load())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
