package store

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
)

// AppliedLedger records, for every input file a template's migrate runs
// applied, the run (batch) that applied it; only the batches' order
// matters. A cold build without a dump applies every file in one run;
// a build on a restored dump keeps the batches the framework recorded
// (LedgerFromBatches); an incremental build applies the files its
// ancestor lacked in one more run.
//
// Frameworks disagree on the order `rollback N` unwinds: Laravel unwinds
// by batch, then by name; Rails, Ecto and golang-migrate by name alone.
// FindRollbackAncestor uses the batches to pick a step count on which
// both orders unwind the same migrations.
//
// A nil *AppliedLedger means the provenance is unknown; such a template
// is never a rollback ancestor, and a template built on top of it
// inherits the unknown provenance.
type AppliedLedger struct {
	batch map[string]int
}

// NewAppliedLedger returns the ledger of a template whose migrate run
// applied every file in inputs at once.
func NewAppliedLedger(inputs map[string]InputVector) *AppliedLedger {
	return (&AppliedLedger{batch: map[string]int{}}).Advance(inputs)
}

// LedgerFromBatches returns the ledger of a template whose framework
// recorded the batch of every migration it applied, keyed by migration
// name; nameOf maps an input path to that name. It is the ledger of a
// database restored from a dump, whose migrations treeman did not
// apply itself. Every input outside lockfileGlobs must have a recorded
// batch, and every recorded migration no input names must come before
// all of the inputs' in batch order: a rollback reaches it otherwise,
// and nothing on disk says how to unwind it. The error says which
// condition failed.
func LedgerFromBatches(
	inputs map[string]InputVector,
	lockfileGlobs map[string]struct{},
	batches map[string]int,
	nameOf func(path string) string,
) (*AppliedLedger, error) {
	ledgerInputs := withoutGlobs(inputs, lockfileGlobs)
	out := make(map[string]int, vectorTotalLen(ledgerInputs))
	named := make(map[string]struct{}, len(out))
	var first *batchedName
	for _, v := range ledgerInputs {
		for _, fh := range v {
			name := nameOf(fh.Path)
			b, ok := batches[name]
			if !ok {
				return nil, fmt.Errorf("the migration ledger has no row for input %s (migration %q)", fh.Path, name)
			}
			out[fh.Path] = b
			named[name] = struct{}{}
			if bn := (batchedName{b, name}); first == nil || bn.less(*first) {
				first = &bn
			}
		}
	}
	if first == nil {
		return &AppliedLedger{batch: out}, nil
	}
	for _, name := range slices.Sorted(maps.Keys(batches)) {
		if _, ok := named[name]; ok {
			continue
		}
		if bn := (batchedName{batches[name], name}); !bn.less(*first) {
			return nil, fmt.Errorf(
				"the migration ledger records %q in batch %d, which no input names, after input migration %q in batch %d",
				name,
				bn.batch,
				first.name,
				first.batch,
			)
		}
	}
	return &AppliedLedger{batch: out}, nil
}

type batchedName struct {
	batch int
	name  string
}

func (a batchedName) less(b batchedName) bool {
	if a.batch != b.batch {
		return a.batch < b.batch
	}
	return a.name < b.name
}

// Advance returns the ledger after one more migrate run against inputs:
// every file l already holds keeps its batch, every other input file
// lands in a new batch after the newest one. A nil ledger stays nil.
func (l *AppliedLedger) Advance(inputs map[string]InputVector) *AppliedLedger {
	if l == nil {
		return nil
	}
	out := make(map[string]int, len(l.batch)+vectorTotalLen(inputs))
	next := 0
	for p, b := range l.batch {
		out[p] = b
		next = max(next, b+1)
	}
	for _, v := range inputs {
		for _, fh := range v {
			if _, ok := out[fh.Path]; !ok {
				out[fh.Path] = next
			}
		}
	}
	return &AppliedLedger{batch: out}
}

// retain returns the ledger restricted to the paths of entries.
func (l *AppliedLedger) retain(entries []FileHash) *AppliedLedger {
	out := make(map[string]int, len(entries))
	for _, fh := range entries {
		out[fh.Path] = l.batch[fh.Path]
	}
	return &AppliedLedger{batch: out}
}

// batchOrder returns entries ordered the way a batch-tracking framework
// applied them: by batch, then by basename, then by path. ok is false
// when an entry has no recorded batch.
func (l *AppliedLedger) batchOrder(entries []FileHash) (ordered []FileHash, ok bool) {
	for _, fh := range entries {
		if _, has := l.batch[fh.Path]; !has {
			return nil, false
		}
	}
	ordered = slices.Clone(entries)
	sort.SliceStable(ordered, func(i, j int) bool {
		bi, bj := l.batch[ordered[i].Path], l.batch[ordered[j].Path]
		if bi != bj {
			return bi < bj
		}
		return lessByBasename(ordered[i], ordered[j])
	})
	return ordered, true
}

// MarshalJSON encodes the ledger as an object of path to batch index.
func (l *AppliedLedger) MarshalJSON() ([]byte, error) {
	return json.Marshal(l.batch)
}

// UnmarshalJSON decodes the object written by MarshalJSON.
func (l *AppliedLedger) UnmarshalJSON(b []byte) error {
	var m map[string]int
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("decode applied ledger: %w", err)
	}
	if m == nil {
		m = map[string]int{}
	}
	l.batch = m
	return nil
}

// agreedUnwindLen returns the smallest step count, at least minSteps, at
// which the last steps entries of byName and of byBatch are the same
// migrations. Unwinding that many leaves the same applied set whichever
// order the framework unwinds in. byName and byBatch hold the same
// entries, so the full length always agrees.
func agreedUnwindLen(byName, byBatch []FileHash, minSteps int) int {
	n := len(byName)
	diff := make(map[string]int, n)
	unmatched := 0
	bump := func(path string, delta int) {
		before := diff[path]
		after := before + delta
		diff[path] = after
		switch {
		case before == 0:
			unmatched++
		case after == 0:
			unmatched--
		}
	}
	for steps := range n {
		if steps >= minSteps && unmatched == 0 {
			return steps
		}
		bump(byName[n-1-steps].Path, 1)
		bump(byBatch[n-1-steps].Path, -1)
	}
	return n
}
