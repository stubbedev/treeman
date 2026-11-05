// Package restorestage names the private databases a restore builds a
// clone in and parks the replaced target under, and recognises the ones
// a crashed run left behind.
package restorestage

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"time"
)

// Prefix starts the name of every restore stage. The `_tm` family keeps
// stages out of every worktree's database prefix and out of the `_tm_`
// template namespace the orphan probe audits.
const Prefix = "_tmrs_"

// MaxAge is how old a stage must be before a restore reaps it as the
// leftover of a crashed run. A live restore finishes within minutes, so
// a day leaves no room for reaping one that is still running.
const MaxAge = 24 * time.Hour

var stageName = regexp.MustCompile(`^_tmrs_([0-9a-f]{1,15})_[0-9a-f]{16}_[no]$`)

// Stage names the two private databases of one restore: Incoming holds
// the clone being built, Retired receives the target's previous
// contents at the swap. Both carry the creation time (unix seconds, hex)
// so a later restore can tell a crashed run's leftovers from a live one.
type Stage struct{ Incoming, Retired string }

// New returns a fresh, unique Stage created at now.
func New(now time.Time) Stage {
	var id [8]byte
	_, _ = rand.Read(id[:])
	base := Prefix + strconv.FormatInt(now.Unix(), 16) + "_" + hex.EncodeToString(id[:])
	return Stage{Incoming: base + "_n", Retired: base + "_o"}
}

// Is reports whether name is a restore stage name.
func Is(name string) bool {
	return stageName.MatchString(name)
}

// Stale returns the names among names that are restore stages created
// more than MaxAge before now. Names that merely share the prefix are
// never returned.
func Stale(names []string, now time.Time) []string {
	var out []string
	for _, n := range names {
		m := stageName.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		sec, err := strconv.ParseInt(m[1], 16, 64)
		if err != nil {
			continue
		}
		if now.Sub(time.Unix(sec, 0)) > MaxAge {
			out = append(out, n)
		}
	}
	return out
}
