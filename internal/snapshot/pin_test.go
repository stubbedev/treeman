package snapshot

import (
	"sync"
	"testing"
)

func TestPinRefcounts(t *testing.T) {
	const fp = "abc123"
	if IsPinned(fp) {
		t.Fatalf("clean state: fp pinned before any Pin call")
	}
	r1 := Pin(fp)
	if !IsPinned(fp) {
		t.Fatalf("after Pin: expected IsPinned=true")
	}
	r2 := Pin(fp)
	r1()
	if !IsPinned(fp) {
		t.Fatalf("after first release: expected IsPinned=true (refcount=1)")
	}
	r2()
	if IsPinned(fp) {
		t.Fatalf("after second release: expected IsPinned=false")
	}
}

func TestPinReleaseIdempotent(t *testing.T) {
	const fp = "idemp"
	r := Pin(fp)
	r()
	r() // second call must be a no-op, not double-decrement
	if IsPinned(fp) {
		t.Fatalf("idempotent release should not flip pin back on")
	}
	// And the count must stay clean so the next Pin/release pair works.
	r2 := Pin(fp)
	if !IsPinned(fp) {
		t.Fatalf("fresh pin after idempotent double-release didn't take")
	}
	r2()
	if IsPinned(fp) {
		t.Fatalf("release after idempotent sequence didn't clear")
	}
}

func TestPinEmptyFingerprintNoOp(t *testing.T) {
	release := Pin("")
	if IsPinned("") {
		t.Fatalf("empty fingerprint must never report as pinned")
	}
	release() // must not panic
}

func TestPinConcurrent(t *testing.T) {
	const fp = "concurrent"
	const n = 64
	var wg sync.WaitGroup
	wg.Add(n)
	releases := make(chan func(), n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			releases <- Pin(fp)
		}()
	}
	wg.Wait()
	close(releases)
	if !IsPinned(fp) {
		t.Fatalf("n concurrent pins: expected IsPinned=true")
	}
	for r := range releases {
		r()
	}
	if IsPinned(fp) {
		t.Fatalf("after all releases: expected IsPinned=false")
	}
}
