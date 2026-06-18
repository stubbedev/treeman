package snapshot

import "sync"

// Pin guards a snapshot fingerprint against the automated GC sweeps
// (EvictExcess, SweepBySource, SweepByAge, SweepBySize). The race it
// closes: a cache-hit prepare looks up a template, starts restoring
// it / fanning it out into N test-clone DBs, and meanwhile a
// concurrent cold-build for an unrelated database finishes and
// triggers EvictExcess. Without a pin, EvictExcess can pick that
// fingerprint as LRU and DROP the template mid-restore — the
// in-flight prepare then sees `Unknown database '_tm_...'`.
//
// The pin is process-local (map under a mutex). That's intentional:
// only one treemand process owns its engine state at a time, and a
// cross-process race would be on different engines anyway. User-
// explicit purges (`treeman snapshot purge`, mcp.snapshots_purge)
// deliberately do NOT consult pins — when a human asks to drop a
// template they get the drop, even if it stomps an in-flight run.
var (
	pinMu    sync.Mutex
	pinCount = map[string]int{}
)

// Pin marks `fingerprint` as in-use and returns a release func.
// Call defer-style; the release is idempotent. Empty fingerprints
// are tolerated as no-ops so callers don't need a guard at every
// site that may or may not have a fingerprint yet.
func Pin(fingerprint string) (release func()) {
	if fingerprint == "" {
		return func() {}
	}
	pinMu.Lock()
	pinCount[fingerprint]++
	pinMu.Unlock()
	var released bool
	return func() {
		pinMu.Lock()
		defer pinMu.Unlock()
		if released {
			return
		}
		released = true
		if pinCount[fingerprint] <= 1 {
			delete(pinCount, fingerprint)
		} else {
			pinCount[fingerprint]--
		}
	}
}

// IsPinned reports whether any caller currently holds a pin on
// `fingerprint`. Used by the GC sweeps to filter their candidate
// lists.
func IsPinned(fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	pinMu.Lock()
	defer pinMu.Unlock()
	return pinCount[fingerprint] > 0
}
