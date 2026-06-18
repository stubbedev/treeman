package snapshot

import (
	"strconv"
	"strings"
)

// PrewarmSuffix joins a template name to its pre-warmed spare-clone
// slot names: `<template>_spare<slot>`. Spares are fully-restored
// copies of the template kept idle so a cache-hit prepare can claim
// one via a constant-time rename instead of paying a full restore.
// The suffix lives here (not in prepare) so the GC sweeps — which drop
// a template's whole spare family alongside it — share one definition.
const PrewarmSuffix = "_spare"

// SpareName returns the database name for `template`'s spare slot.
// Slots are 1-based so the names read naturally next to test-clone
// `_w1.._wN` fanout names.
func SpareName(template string, slot int) string {
	return template + PrewarmSuffix + strconv.Itoa(slot)
}

// SpareSlot parses the slot index out of a spare database name
// produced by SpareName. ok is false for names that aren't spares of
// `template` (wrong prefix, empty/non-numeric slot).
func SpareSlot(name, template string) (slot int, ok bool) {
	rest, found := strings.CutPrefix(name, template+PrewarmSuffix)
	if !found || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}
