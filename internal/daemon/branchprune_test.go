package daemon

import (
	"context"
	"os"
	"path/filepath"
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
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
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
