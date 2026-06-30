package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stubbedev/treeman/internal/gitcmd"
)

// makeFeatureGone builds the shared fixture: a `feature` branch in `work`
// that was pushed (so it has a tracked upstream) and then deleted on the
// remote, so after `fetch --prune` its upstream reads [gone]. `integrate`
// chooses how feature's work lands on origin/main before the remote branch
// is removed:
//
//	"ff"     — fast-forward origin/main to feature (commits become ancestors).
//	"squash" — land feature's tree as one new commit (individual commits do
//	           NOT enter main's history — the squash-merge case).
//	"none"   — leave origin/main untouched (feature is genuinely unmerged).
//
// Leaves `work` on `main` unless stayOnFeature is set.
func makeFeatureGone(t *testing.T, integrate string, stayOnFeature bool) string {
	t.Helper()
	work, origin := makeClone(t)

	gitRun(t, work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "feat.txt"), []byte("feature work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "feat.txt")
	gitRun(t, work, "commit", "-q", "-m", "feat")
	gitRun(t, work, "push", "-q", "-u", "origin", "feature")

	switch integrate {
	case "ff":
		push := filepath.Join(t.TempDir(), "push")
		gitRun(t, "", "clone", "-q", origin, push)
		gitRun(t, push, "config", "user.email", "t@t")
		gitRun(t, push, "config", "user.name", "t")
		gitRun(t, push, "checkout", "-q", "main")
		gitRun(t, push, "merge", "-q", "--ff-only", "origin/feature")
		gitRun(t, push, "push", "-q", "origin", "main")
	case "squash":
		push := filepath.Join(t.TempDir(), "push")
		gitRun(t, "", "clone", "-q", origin, push)
		gitRun(t, push, "config", "user.email", "t@t")
		gitRun(t, push, "config", "user.name", "t")
		gitRun(t, push, "checkout", "-q", "main")
		gitRun(t, push, "merge", "-q", "--squash", "origin/feature")
		gitRun(t, push, "commit", "-q", "-m", "squash feature")
		gitRun(t, push, "push", "-q", "origin", "main")
	case "none":
		// origin/main untouched — feature stays unmerged.
	}

	gitRun(t, origin, "branch", "-D", "feature")
	if !stayOnFeature {
		gitRun(t, work, "checkout", "-q", "main")
	}
	gitRun(t, work, "fetch", "--prune", "-q", "origin")
	return work
}

func featureExists(work string) bool {
	return gitcmd.Exists(context.Background(), work, "refs/heads/feature")
}

func listed(xs []string, want string) bool {
	return slices.Contains(xs, want)
}

func requireDeleted(t *testing.T, work string, got []string) {
	t.Helper()
	if !listed(got, "feature") {
		t.Errorf("pruneGoneLocals returned %v, want it to include \"feature\"", got)
	}
	if featureExists(work) {
		t.Error("feature ref still exists; expected it deleted")
	}
}

func requireKept(t *testing.T, work string, got []string) {
	t.Helper()
	if listed(got, "feature") {
		t.Errorf("pruneGoneLocals deleted \"feature\" (returned %v); expected it kept", got)
	}
	if !featureExists(work) {
		t.Error("feature ref was deleted; expected it kept")
	}
}

// A merged branch whose upstream is gone is pruned.
func TestPruneGoneLocals_DeletesMerged(t *testing.T) {
	requireGitAutofetch(t)
	work := makeFeatureGone(t, "ff", false)
	requireDeleted(t, work, pruneGoneLocals(context.Background(), work))
}

// A squash-merged branch (commits absent from main, tree present) is pruned —
// and we assert the squash detection path is what's exercised, not ancestry.
func TestPruneGoneLocals_DeletesSquashMerged(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()
	work := makeFeatureGone(t, "squash", false)
	if gitcmd.RunOptional(ctx, work, "merge-base", "--is-ancestor", "feature", "origin/main") == nil {
		t.Fatal("precondition: feature is an ancestor of origin/main — not exercising the squash path")
	}
	if !squashContained(ctx, work, "origin/main", "feature") {
		t.Fatal("squashContained should detect the squash-merged branch")
	}
	requireDeleted(t, work, pruneGoneLocals(ctx, work))
}

// Regression for the `git cherry <upstream> <synth> <base>` limit arg: the
// squash commit must still be detected when origin/main has advanced past it
// (commits landed after the squash). The <base> limit narrows the patch-id
// range for CPU, and must not exclude the matching squash commit — which sits
// between merge-base and tip, inside the limited range.
func TestPruneGoneLocals_DeletesSquashMergedWithLaterCommits(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()
	work := makeFeatureGone(t, "squash", false)

	// Advance origin/main with an unrelated commit after the squash, then
	// fetch so the local feature is compared against a moved-on default.
	origin := gitOut(t, work, "remote", "get-url", "origin")
	push := filepath.Join(t.TempDir(), "push")
	gitRun(t, "", "clone", "-q", origin, push)
	gitRun(t, push, "config", "user.email", "t@t")
	gitRun(t, push, "config", "user.name", "t")
	gitRun(t, push, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(push, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, push, "add", "later.txt")
	gitRun(t, push, "commit", "-q", "-m", "later unrelated work")
	gitRun(t, push, "push", "-q", "origin", "main")
	gitRun(t, work, "fetch", "--prune", "-q", "origin")

	if !squashContained(ctx, work, "origin/main", "feature") {
		t.Fatal("squashContained should still detect the squash after main advanced past it")
	}
	requireDeleted(t, work, pruneGoneLocals(ctx, work))
}

// Several squash-merged-gone branches with DIFFERENT merge-bases plus one
// unmerged-gone branch are resolved in a single sweep: the batched path scans
// defRef's patch-ids once (bounded by the oldest base) and matches every
// suspect, deleting the squashed ones while keeping the unmerged one. Guards
// the multi-suspect efficiency path that replaced the per-branch `git cherry`.
func TestPruneGoneLocals_BatchedMultiSuspect(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()
	work, origin := makeClone(t)

	squashMergeGone := func(branch, file string) {
		gitRun(t, work, "checkout", "-q", "main")
		gitRun(t, work, "checkout", "-q", "-b", branch)
		if err := os.WriteFile(filepath.Join(work, file), []byte(file+" work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "add", file)
		gitRun(t, work, "commit", "-q", "-m", branch)
		gitRun(t, work, "push", "-q", "-u", "origin", branch)

		push := filepath.Join(t.TempDir(), "push")
		gitRun(t, "", "clone", "-q", origin, push)
		gitRun(t, push, "config", "user.email", "t@t")
		gitRun(t, push, "config", "user.name", "t")
		gitRun(t, push, "checkout", "-q", "main")
		gitRun(t, push, "merge", "-q", "--squash", "origin/"+branch)
		gitRun(t, push, "commit", "-q", "-m", "squash "+branch)
		gitRun(t, push, "push", "-q", "origin", "main")
		gitRun(t, origin, "branch", "-D", branch)
	}

	// feature1 forks from the seed commit; feature2 forks after feature1's
	// squash landed — so the two have distinct merge-bases and the shared
	// scan must span back to the oldest of them.
	gitRun(t, work, "fetch", "-q", "origin")
	squashMergeGone("feature1", "f1.txt")
	gitRun(t, work, "fetch", "-q", "origin")
	gitRun(t, work, "checkout", "-q", "main")
	gitRun(t, work, "merge", "-q", "--ff-only", "origin/main")
	squashMergeGone("feature2", "f2.txt")

	// feature3: gone upstream but never integrated — must be kept.
	gitRun(t, work, "checkout", "-q", "main")
	gitRun(t, work, "checkout", "-q", "-b", "feature3")
	if err := os.WriteFile(filepath.Join(work, "f3.txt"), []byte("f3 work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "f3.txt")
	gitRun(t, work, "commit", "-q", "-m", "feature3")
	gitRun(t, work, "push", "-q", "-u", "origin", "feature3")
	gitRun(t, origin, "branch", "-D", "feature3")

	gitRun(t, work, "checkout", "-q", "main")
	gitRun(t, work, "fetch", "--prune", "-q", "origin")

	got := pruneGoneLocals(ctx, work)
	for _, b := range []string{"feature1", "feature2"} {
		if !listed(got, b) {
			t.Errorf("pruneGoneLocals returned %v, want it to delete %q", got, b)
		}
		if gitcmd.Exists(ctx, work, "refs/heads/"+b) {
			t.Errorf("%s ref still exists; expected it deleted", b)
		}
	}
	if listed(got, "feature3") {
		t.Errorf("pruneGoneLocals deleted unmerged \"feature3\" (returned %v)", got)
	}
	if !gitcmd.Exists(ctx, work, "refs/heads/feature3") {
		t.Error("feature3 ref was deleted; expected it kept")
	}
}

// A squash-merged-gone branch whose fork point is farther behind defRef than
// maxSquashScanCommits is KEPT, not reaped: the bounded scan must refuse the
// far-forked range that ballooned memory/CPU every tick. Same fixture as the
// squash-deletes test, only the ceiling is lowered to 0 so the one squash
// commit on main counts as "too far behind".
func TestPruneGoneLocals_SkipsFarForkedSuspect(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()
	work := makeFeatureGone(t, "squash", false)

	saved := maxSquashScanCommits
	maxSquashScanCommits = 0
	t.Cleanup(func() { maxSquashScanCommits = saved })

	requireKept(t, work, pruneGoneLocals(ctx, work))
}

// commitsBehind counts defRef commits past base (base..defRef) and nothing else.
func TestCommitsBehind(t *testing.T) {
	ctx := context.Background()
	work, _ := makeClone(t)
	base := gitOut(t, work, "rev-parse", "HEAD")
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(work, "c.txt"), []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "add", "c.txt")
		gitRun(t, work, "commit", "-q", "-m", "c")
	}
	if n, ok := commitsBehind(ctx, work, base, "HEAD"); !ok || n != 3 {
		t.Errorf("commitsBehind = %d, %v; want 3, true", n, ok)
	}
}

// A branch whose upstream is gone but whose work was NOT integrated is kept —
// this is the data-safety guarantee.
func TestPruneGoneLocals_KeepsUnmergedGone(t *testing.T) {
	requireGitAutofetch(t)
	work := makeFeatureGone(t, "none", false)
	requireKept(t, work, pruneGoneLocals(context.Background(), work))
}

// A branch whose upstream still exists is untouched (not [gone]).
func TestPruneGoneLocals_KeepsLiveUpstream(t *testing.T) {
	requireGitAutofetch(t)
	work, _ := makeClone(t)
	gitRun(t, work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "feat.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "feat.txt")
	gitRun(t, work, "commit", "-q", "-m", "feat")
	gitRun(t, work, "push", "-q", "-u", "origin", "feature")
	gitRun(t, work, "checkout", "-q", "main")
	gitRun(t, work, "fetch", "--prune", "-q", "origin")
	requireKept(t, work, pruneGoneLocals(context.Background(), work))
}

// A local-only branch that never had an upstream is untouched.
func TestPruneGoneLocals_KeepsLocalOnly(t *testing.T) {
	requireGitAutofetch(t)
	work, _ := makeClone(t)
	gitRun(t, work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "feat.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "feat.txt")
	gitRun(t, work, "commit", "-q", "-m", "feat")
	gitRun(t, work, "checkout", "-q", "main")
	requireKept(t, work, pruneGoneLocals(context.Background(), work))
}

// A merged-and-gone branch that is currently checked out is skipped while
// checked out, then pruned once the worktree moves off it.
func TestPruneGoneLocals_SkipsCheckedOut(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()
	work := makeFeatureGone(t, "ff", true) // left on feature
	requireKept(t, work, pruneGoneLocals(ctx, work))
	gitRun(t, work, "checkout", "-q", "main")
	requireDeleted(t, work, pruneGoneLocals(ctx, work))
}
