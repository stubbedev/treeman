package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// auto_fetch.interval_minutes maps minutes → ticker duration, with a
// sub-minute clamp so interval_minutes: 0 can't spin a tight ticker.
func TestAutoFetchInterval(t *testing.T) {
	cases := []struct {
		minutes uint32
		want    time.Duration
	}{
		{0, time.Minute}, // clamp: 0 would hammer every remote
		{1, time.Minute}, // exact floor
		{15, 15 * time.Minute},
		{60, time.Hour},
	}
	for _, c := range cases {
		got := autoFetchInterval(config.AutoFetchConfig{IntervalMinutes: c.minutes})
		if got != c.want {
			t.Errorf("interval_minutes=%d → %v, want %v", c.minutes, got, c.want)
		}
	}
}

// requireGit skips when the host doesn't have git on PATH.
func requireGitAutofetch(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// gitRun shells `git -C dir args...` and t.Fatals on any error.
// Keeping the fixture inline keeps the test self-contained and lets
// us iterate without growing an internal/testutil package.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", full, err, out)
	}
}

// gitOut shells `git -C dir args...` and returns trimmed stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", full, err, out)
	}
	return strings.TrimSpace(string(out))
}

// makeClone constructs an "upstream + clone" pair under tmp:
//
//	tmp/origin.git  — bare repo, seeded with one commit on `main`.
//	tmp/work        — clone of origin.git, branch `main` tracking origin/main.
//
// Returns the working tree path.
func makeClone(t *testing.T) (work, origin string) {
	t.Helper()
	tmp := t.TempDir()
	origin = filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")

	gitRun(t, "", "init", "-q", "-b", "main", "--bare", origin)
	gitRun(t, "", "init", "-q", "-b", "main", seed)
	gitRun(t, seed, "config", "user.email", "t@t")
	gitRun(t, seed, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", "README")
	gitRun(t, seed, "commit", "-q", "-m", "init")
	gitRun(t, seed, "remote", "add", "origin", origin)
	gitRun(t, seed, "push", "-q", "origin", "main")

	work = filepath.Join(tmp, "work")
	gitRun(t, "", "clone", "-q", origin, work)
	gitRun(t, work, "config", "user.email", "t@t")
	gitRun(t, work, "config", "user.name", "t")
	return work, origin
}

// advanceOrigin pushes one new commit to `main` on the bare origin
// via a throwaway clone — simulates another contributor moving the
// upstream forward while our work clone holds still.
func advanceOrigin(t *testing.T, origin string) {
	t.Helper()
	tmp := t.TempDir()
	push := filepath.Join(tmp, "push")
	gitRun(t, "", "clone", "-q", origin, push)
	gitRun(t, push, "config", "user.email", "t@t")
	gitRun(t, push, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(push, "next"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, push, "add", "next")
	gitRun(t, push, "commit", "-q", "-m", "next")
	gitRun(t, push, "push", "-q", "origin", "main")
}

// TestAutoFetchAdvancesCleanWorktree verifies the happy path: clean
// tree, upstream has a new commit, sweep runs → HEAD advances.
func TestAutoFetchAdvancesCleanWorktree(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()

	work, origin := makeClone(t)
	advanceOrigin(t, origin)

	headBefore := gitOut(t, work, "rev-parse", "HEAD")
	originHead := gitOut(t, work, "ls-remote", "origin", "main")
	originSha := strings.Fields(originHead)[0]
	if headBefore == originSha {
		t.Fatalf("precondition: clone already at origin/main")
	}

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)

	repoID, err := s.EnsureRepo(ctx, work, "work")
	if err != nil {
		t.Fatal(err)
	}

	runAutoFetchSweep(ctx, st)

	headAfter := gitOut(t, work, "rev-parse", "HEAD")
	if headAfter != originSha {
		t.Errorf("HEAD = %s, want %s (origin/main)", headAfter, originSha)
	}
	_ = repoID
}

// TestAutoFetchSkipsDirtyWorktree verifies the safety guarantee:
// modified tracked file → pull is skipped → HEAD doesn't move.
func TestAutoFetchSkipsDirtyWorktree(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()

	work, origin := makeClone(t)
	advanceOrigin(t, origin)

	// Dirty the tracked file. `git status --porcelain -uno` will
	// report this, so tryFFPull must skip merge --ff-only.
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	headBefore := gitOut(t, work, "rev-parse", "HEAD")

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)
	if _, err := s.EnsureRepo(ctx, work, "work"); err != nil {
		t.Fatal(err)
	}

	runAutoFetchSweep(ctx, st)

	headAfter := gitOut(t, work, "rev-parse", "HEAD")
	if headAfter != headBefore {
		t.Errorf("HEAD moved on dirty tree: before=%s after=%s", headBefore, headAfter)
	}
	// Fetch should still have run — remote-tracking ref must be up to
	// date even though the local branch wasn't moved.
	remote := gitOut(t, work, "rev-parse", "refs/remotes/origin/main")
	wantRemote := strings.Fields(gitOut(t, work, "ls-remote", "origin", "main"))[0]
	if remote != wantRemote {
		t.Errorf("fetch did not refresh remote ref: got=%s want=%s", remote, wantRemote)
	}
}

// TestAutoFetchSkipsNonFastForward verifies divergence is left alone:
// local has a unique commit AND upstream has a unique commit → ff
// would refuse → HEAD doesn't move.
func TestAutoFetchSkipsNonFastForward(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()

	work, origin := makeClone(t)

	// Local commit that origin/main doesn't have.
	if err := os.WriteFile(filepath.Join(work, "local"), []byte("l\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "local")
	gitRun(t, work, "commit", "-q", "-m", "local")
	localHead := gitOut(t, work, "rev-parse", "HEAD")

	// Origin commit that the local clone doesn't have.
	advanceOrigin(t, origin)

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)
	if _, err := s.EnsureRepo(ctx, work, "work"); err != nil {
		t.Fatal(err)
	}

	runAutoFetchSweep(ctx, st)

	headAfter := gitOut(t, work, "rev-parse", "HEAD")
	if headAfter != localHead {
		t.Errorf("HEAD moved on diverged branch: before=%s after=%s", localHead, headAfter)
	}
}
