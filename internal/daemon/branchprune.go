package daemon

import (
	"context"
	"log/slog"
	"strings"

	"github.com/stubbedev/treeman/internal/gitcmd"
)

// pruneGoneLocals deletes local branches whose remote upstream was deleted
// ([gone]) AND which are provably integrated into the default branch. The
// auto-fetch loop calls it after `fetch --prune` so merged branches don't
// have to be cleaned up by hand — and so the branch_scoped durable databases
// they left behind can be reaped (the caller does that with the returned
// names).
//
// "Provably integrated" means either the branch tip is an ancestor of the
// default branch (fast-forward / merge-commit) or its cumulative diff is
// already present there (squash-merge, see squashContained). A branch that
// can't be proven merged keeps both its ref and its durable — genuinely
// unmerged work is never force-deleted.
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

	var deleted []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
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
		if !safeToDelete(ctx, repoRoot, defRef, name) {
			continue // genuinely unmerged — keep the branch and its durable
		}
		// Containment is proven above, so -D (not -d, whose merged-check is
		// against HEAD — which may not be the default branch).
		if err := gitcmd.RunOptional(ctx, repoRoot, "branch", "-D", name); err != nil {
			slog.Warn("branch_prune delete", "repo", repoRoot, "branch", name, "err", err)
			continue
		}
		slog.Info("branch_prune deleted merged gone-upstream branch", "repo", repoRoot, "branch", name)
		deleted = append(deleted, name)
	}
	return deleted
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
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "branch refs/heads/"); ok {
			if b := strings.TrimSpace(rest); b != "" {
				set[b] = struct{}{}
			}
		}
	}
	return set
}

// safeToDelete reports whether `branch` is provably already integrated into
// `defRef` (the default branch's remote-tracking ref).
func safeToDelete(ctx context.Context, repoRoot, defRef, branch string) bool {
	// Fast-forward / merge-commit: tip already reachable from the default.
	if gitcmd.RunOptional(ctx, repoRoot, "merge-base", "--is-ancestor", branch, defRef) == nil {
		return true
	}
	return squashContained(ctx, repoRoot, defRef, branch)
}

// squashContained detects a squash-merged branch. It collapses the branch's
// cumulative diff into a single synthetic commit atop the merge-base, then asks
// `git cherry` whether that patch is already present in `defRef`. This is the
// only reliable local signal that a squash-merged-then-deleted branch's work
// actually landed: the squash rewrites history, so the individual commits never
// appear in the default branch and ancestor / `branch -d` checks all miss it.
// Read-only in effect — the synthetic commit is a dangling object git will
// garbage-collect.
func squashContained(ctx context.Context, repoRoot, defRef, branch string) bool {
	base, err := gitcmd.String(ctx, repoRoot, "merge-base", defRef, branch)
	if err != nil || base == "" {
		return false
	}
	tree, err := gitcmd.String(ctx, repoRoot, "rev-parse", branch+"^{tree}")
	if err != nil || tree == "" {
		return false
	}
	synth, err := gitcmd.String(ctx, repoRoot, "commit-tree", tree, "-p", base, "-m", "treeman-squash-probe")
	if err != nil || synth == "" {
		return false
	}
	out, err := gitcmd.Output(ctx, repoRoot, "cherry", defRef, synth)
	if err != nil {
		return false
	}
	// A leading "-" marks the synthetic patch as already present in defRef.
	return strings.HasPrefix(strings.TrimSpace(string(out)), "-")
}
