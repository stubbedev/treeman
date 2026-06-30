package daemon

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/stubbedev/treeman/internal/gitcmd"
)

// maxSquashScanCommits caps how far back of defRef history the squash-detection
// scan will reach. A suspect whose fork point sits more than this many commits
// behind defRef is skipped (the branch is kept, never force-deleted): the only
// way to "prove" such a branch squash-merged is to `log -p | patch-id` the
// entire range since its fork, which on a busy monorepo means materializing a
// year of diffs (40k+ commits → multi-GB RSS, minutes of CPU) — and because a
// gone-but-unmergeable ancient branch never resolves, that scan re-fired every
// auto-fetch tick. A branch that old is not a realistic recent-merge candidate;
// declining to auto-reap it is the cheap, safe call.
// ponytail: flat ceiling; make it config-driven if a repo legitimately merges
// branches forked >this many commits back and wants them auto-pruned. var (not
// const) only so tests can lower it without building thousands of commits.
var maxSquashScanCommits = 5000

// pruneGoneLocals deletes local branches whose remote upstream was deleted
// ([gone]) AND which are provably integrated into the default branch. The
// auto-fetch loop calls it after `fetch --prune` so merged branches don't
// have to be cleaned up by hand — and so the branch_scoped durable databases
// they left behind can be reaped (the caller does that with the returned
// names).
//
// "Provably integrated" means either the branch tip is an ancestor of the
// default branch (fast-forward / merge-commit) or its cumulative diff is
// already present there (squash-merge, see squashMergedSuspects). A branch
// that can't be proven merged keeps both its ref and its durable — genuinely
// unmerged work is never force-deleted.
//
// The work splits in two so the cost scales with what's actually needed:
//   - Ancestor check (`merge-base --is-ancestor`) is cheap and resolves the
//     common case (ff / merge-commit merges) with no diff work — done inline.
//   - Squash detection needs patch-ids, the expensive part. Every remaining
//     suspect is batched into ONE shared defRef patch-id pass instead of a
//     per-branch `git cherry` that re-walked defRef's history once per branch
//     (the O(suspects × defRef-history) blowup that pinned a core every tick).
//
// Guards: never deletes the default branch itself, never a branch checked out
// in any worktree (git refuses that anyway), and never a branch that never had
// an upstream (the [gone] filter requires a tracking ref that was deleted).
func pruneGoneLocals(ctx context.Context, repoRoot string) []string {
	defRef, ok := defaultRemoteRef(ctx, repoRoot)
	if !ok {
		// No trustworthy default branch to compare against → prove nothing
		// → delete nothing.
		return nil
	}
	defName := strings.TrimPrefix(defRef, "origin/")

	out, err := gitcmd.Output(ctx, repoRoot, "for-each-ref",
		"--format=%(refname:short)%09%(upstream:track)", "refs/heads/")
	if err != nil {
		slog.Warn("branch_prune list refs", "repo", repoRoot, "err", err)
		return nil
	}
	checkedOut := checkedOutBranches(ctx, repoRoot)

	var deleted, suspects []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		name, track, _ := strings.Cut(line, "\t")
		if name == "" || name == defName || track != "[gone]" {
			continue
		}
		if _, busy := checkedOut[name]; busy {
			continue
		}
		// Fast path: tip already reachable from the default branch
		// (fast-forward / merge-commit). No patch-id work needed.
		if gitcmd.RunOptional(ctx, repoRoot, "merge-base", "--is-ancestor", name, defRef) == nil {
			deleteGoneLocal(ctx, repoRoot, name, &deleted)
			continue
		}
		suspects = append(suspects, name)
	}

	// Squash-merge detection for whatever the ancestor check didn't resolve.
	// One shared defRef scan covers every suspect; genuinely unmerged
	// suspects fall through and are kept.
	for _, name := range squashMergedSuspects(ctx, repoRoot, defRef, suspects) {
		deleteGoneLocal(ctx, repoRoot, name, &deleted)
	}
	return deleted
}

// deleteGoneLocal force-deletes a provably-integrated branch and records it.
// -D (not -d) because containment is already proven against the default
// branch — -d's merged-check is against HEAD, which may not be the default.
func deleteGoneLocal(ctx context.Context, repoRoot, name string, deleted *[]string) {
	if err := gitcmd.RunOptional(ctx, repoRoot, "branch", "-D", name); err != nil {
		slog.Warn("branch_prune delete", "repo", repoRoot, "branch", name, "err", err)
		return
	}
	slog.Info("branch_prune deleted merged gone-upstream branch", "repo", repoRoot, "branch", name)
	*deleted = append(*deleted, name)
}

// defaultRemoteRef returns the remote-tracking ref of the repo's default
// branch (e.g. "origin/master") — the freshest mainline state right after
// `fetch`, and the ref merge-status is proven against. ok=false when it can't
// be resolved, so the caller declines to prune rather than guess against the
// wrong target.
func defaultRemoteRef(ctx context.Context, repoRoot string) (string, bool) {
	if s, err := gitcmd.String(ctx, repoRoot, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && s != "" {
		return s, true
	}
	for _, c := range []string{"origin/main", "origin/master"} {
		if gitcmd.Exists(ctx, repoRoot, c) {
			return c, true
		}
	}
	return "", false
}

// checkedOutBranches returns the set of branch names currently checked out in
// any of the repo's worktrees (including the main checkout). `git branch -D`
// refuses these; pre-skipping keeps the logs free of expected failures.
func checkedOutBranches(ctx context.Context, repoRoot string) map[string]struct{} {
	set := map[string]struct{}{}
	out, err := gitcmd.Output(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return set
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			if b := strings.TrimSpace(rest); b != "" {
				set[b] = struct{}{}
			}
		}
	}
	return set
}

// squashMergedSuspects returns the subset of `suspects` that were squash-merged
// into defRef. A squash rewrites history, so the branch's individual commits
// never enter defRef and the ancestor check misses — the only local signal is
// that the branch's cumulative diff (merge-base..branch) is already present in
// defRef as a single commit with the same patch-id.
//
// The expensive half — patch-id'ing defRef's commits — is done ONCE for the
// whole suspect set, over the range since the oldest suspect merge-base, then
// each suspect's cumulative-diff patch-id is a map lookup. This replaces the
// previous per-suspect `git cherry`, each of which independently patch-id'd
// defRef's history from its own merge-base: O(suspects × defRef-since-base)
// CPU every auto-fetch tick, the source of the 100% spike on a many-branch
// repo. Now it's O(defRef-since-oldest-base + suspects), and zero when there
// are no suspects at all.
//
// Suspects whose merge-base or diff patch-id can't be computed are omitted
// (kept) — same conservative "never force-delete unproven work" stance.
// Read-only: no refs or objects are written.
func squashMergedSuspects(ctx context.Context, repoRoot, defRef string, suspects []string) []string {
	if len(suspects) == 0 {
		return nil
	}

	type candidate struct {
		name    string
		patchID string
	}
	var cands []candidate
	var bases []string
	for _, name := range suspects {
		base, err := gitcmd.String(ctx, repoRoot, "merge-base", defRef, name)
		if err != nil || base == "" {
			continue
		}
		// Skip branches whose fork point is far behind defRef. Their squash
		// (if any) sits anywhere in that whole range, so proving it would
		// require scanning all of it — the cost that pinned a core and 2GB+
		// every tick on a year-stale gone branch. One ancient suspect must
		// not drag the shared scan (bounded by the oldest base) back with it.
		dist, ok := commitsBehind(ctx, repoRoot, base, defRef)
		if !ok || dist > maxSquashScanCommits {
			slog.Debug("branch_prune skip far-forked suspect",
				"repo", repoRoot, "branch", name, "commits_behind", dist)
			continue
		}
		pid := cumulativeDiffPatchID(ctx, repoRoot, base, name)
		if pid == "" {
			continue
		}
		cands = append(cands, candidate{name: name, patchID: pid})
		bases = append(bases, base)
	}
	if len(cands) == 0 {
		return nil
	}

	present := defRefPatchIDIndex(ctx, repoRoot, defRef, bases)
	if present == nil {
		return nil
	}

	var merged []string
	for _, c := range cands {
		if _, ok := present[c.patchID]; ok {
			merged = append(merged, c.name)
		}
	}
	return merged
}

// squashContained reports whether a single branch was squash-merged into
// defRef. Thin wrapper over squashMergedSuspects so the batched path is the
// one source of truth; retained as the unit-test entry point and for callers
// that check one branch.
func squashContained(ctx context.Context, repoRoot, defRef, branch string) bool {
	return slices.Contains(squashMergedSuspects(ctx, repoRoot, defRef, []string{branch}), branch)
}

// cumulativeDiffPatchID returns the patch-id of the whole-branch diff
// base..branch — the same fingerprint `git cherry` compares, computed directly
// so it can be matched against a precomputed defRef index. patch-id normalises
// line offsets, so a squash applied atop a moved-on defRef still matches the
// original cumulative diff. Empty string on any failure (caller keeps the
// branch). Both sides of the comparison use `patch-id --stable`, so the match
// is self-consistent regardless of git's default patch-id algorithm.
func cumulativeDiffPatchID(ctx context.Context, repoRoot, base, branch string) string {
	out, err := gitcmd.PipeOutput(ctx, repoRoot,
		[]string{"diff", base, branch},
		[]string{"patch-id", "--stable"})
	if err != nil {
		return ""
	}
	return firstField(out)
}

// defRefPatchIDIndex builds the set of patch-ids of every non-merge commit in
// defRef newer than the oldest of `bases` — one streamed `git log -p | git
// patch-id` pass that covers every suspect's post-base range. Merge commits
// are excluded: they carry no single cumulative diff and the ancestor
// fast-path already handles merge-commit merges. Returns nil on failure.
func defRefPatchIDIndex(ctx context.Context, repoRoot, defRef string, bases []string) map[string]struct{} {
	logArgs := []string{"log", "-p", "--no-merges", "--format=commit %H", defRef}
	if oldest := oldestCommonAncestor(ctx, repoRoot, bases); oldest != "" {
		// Limit defRef to commits after the oldest fork point — every
		// suspect's squash sits between its own (newer-or-equal) base and
		// defRef's tip, so it stays inside this range.
		logArgs = append(logArgs, "^"+oldest)
	}
	out, err := gitcmd.PipeOutput(ctx, repoRoot, logArgs, []string{"patch-id", "--stable"})
	if err != nil {
		slog.Warn("branch_prune patch-id index", "repo", repoRoot, "err", err)
		return nil
	}
	ids := map[string]struct{}{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if id := firstField([]byte(line)); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}

// commitsBehind returns how many commits defRef has past base (base..defRef) —
// the size of the history the squash scan would have to walk for this suspect.
// Counts commits only (no diffs/trees), so it's cheap even over a year of
// history. ok=false when it can't be computed, which the caller treats as
// "don't risk the scan" and keeps the branch.
func commitsBehind(ctx context.Context, repoRoot, base, defRef string) (int, bool) {
	s, err := gitcmd.String(ctx, repoRoot, "rev-list", "--count", base+".."+defRef)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return n, true
}

// oldestCommonAncestor returns the merge-base of all `commits` — their common
// ancestor, which (since every base is itself an ancestor of defRef) is the
// oldest fork point and thus a safe lower bound for the shared defRef scan.
// Empty string when it can't be resolved, which makes the caller scan all of
// defRef rather than risk excluding a suspect's squash commit.
func oldestCommonAncestor(ctx context.Context, repoRoot string, commits []string) string {
	switch len(commits) {
	case 0:
		return ""
	case 1:
		return commits[0]
	}
	s, err := gitcmd.String(ctx, repoRoot, append([]string{"merge-base"}, commits...)...)
	if err != nil {
		return ""
	}
	return s
}

// firstField returns the first whitespace-delimited token of b — the patch-id
// in `git patch-id` output lines (`<patch-id> <commit-id>`). Empty when b is
// blank.
func firstField(b []byte) string {
	s := strings.TrimSpace(string(b))
	if before, _, found := strings.Cut(s, " "); found {
		return before
	}
	return s
}
