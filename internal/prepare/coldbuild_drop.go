// Helpers for cold-build's pre-build drop step. The bare prefix-LIKE
// in driver.DropMatching is correct for full-worktree teardown (one
// slug owns its whole `<source>_<slug>*` family), but disastrous in
// cold-build for engines where the main-worktree overlay rebinds the
// source name to a bare value that prefixes every other worktree's
// `<source>_<slug>` databases. A main-wt cold build issuing
// DropMatching(`kontainer_testing`) silently nuked sibling worktrees'
// `kontainer_testing_kon_*` source and per-test clone DBs — observed
// in production, no `db_drop` events because cold-build never emitted
// one. This file's helpers anchor those drops to "what this worktree
// actually owns".
package prepare

import (
	"context"
	"fmt"
	"strings"

	"github.com/stubbedev/treeman/internal/store"
)

// emitColdBuildDrop records that cold-build dropped `target` as part
// of pre-build cleanup. Previously this step was silent; an
// over-eager DropMatching wiped sibling worktrees with nothing in
// the event log to point at, which is what made the original
// production incident invisible until a human noticed missing test
// DBs. The event mirrors the shape of TeardownDatabases' `db_drop`
// so log queries that filter on event_type=db_drop catch both
// surfaces.
func emitColdBuildDrop(
	ctx context.Context,
	st *store.Store,
	repoID, worktreeID int64,
	engine, target string,
) {
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtDBDrop,
		fmt.Sprintf("%s: cold-build pre-drop %s", engine, target),
		repoID, worktreeID, "", 0, map[string]any{
			"engine": engine,
			"target": target,
			"reason": "cold_build_pre_drop",
		})
}

// siblingSlugs returns every active worktree slug in repoID except
// selfWtID. Used to filter cold-build's ES drop so indices owned by
// other worktrees (whose names happen to share the current
// worktree's source prefix under the main-wt overlay rebinding) are
// kept.
//
// Returns nil on store error; cold-build then falls back to the
// unfiltered list (worst case = the pre-fix behaviour, best case =
// nothing extra to skip). A logged store error would be noisy on
// every prepare; the caller emits an event if it cares.
func siblingSlugs(ctx context.Context, st *store.Store, repoID, selfWtID int64) []string {
	rows, err := st.ListWorktreesForRepo(ctx, repoID)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, w := range rows {
		if w.ID == selfWtID || w.Slug == "" || w.Deleted {
			continue
		}
		out = append(out, w.Slug)
	}
	return out
}

// nameOwnedByOtherSlug reports whether `name` contains one of `otherSlugs`
// as a whole, delimiter-bounded token, for any slug in `otherSlugs`. A
// slug is "bounded" when the bytes immediately before and after it are
// delimiters — any non-alphanumeric byte (`_`, `:`, `-`, `.`, …) — or the
// string edge. Delimiter-agnostic on purpose: it must work for both
// `<prefix>_<slug>_*` (ES/SQL naming) and `<prefix>:<slug>:*` (redis key
// naming). The boundary matters: slug `a` must not match `_abc` (next byte
// `b` is alphanumeric), and `app_ab_` must not be attributed to slug `a`.
//
// Used as the inverse of the "keep" predicate in es.DropMatchingFiltered /
// redis.DropPrefixFiltered:
// `keep := func(n string) bool { return !nameOwnedByOtherSlug(n, others) }`.
func nameOwnedByOtherSlug(name string, otherSlugs []string) bool {
	for _, s := range otherSlugs {
		if s == "" {
			continue
		}
		idx := 0
		for {
			hit := strings.Index(name[idx:], s)
			if hit < 0 {
				break
			}
			start := idx + hit
			end := start + len(s)
			beforeOK := start == 0 || isNameDelimiter(name[start-1])
			afterOK := end == len(name) || isNameDelimiter(name[end])
			if beforeOK && afterOK {
				return true
			}
			idx = start + 1
		}
	}
	return false
}

// isNameDelimiter reports whether b marks a namespace token boundary — any
// byte that is not an ASCII letter or digit. Slugs are alphanumeric (with
// internal `_`), so a non-alphanumeric byte adjacent to a full-slug match
// is a delimiter regardless of which separator the name template used.
func isNameDelimiter(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return false
	default:
		return true
	}
}
