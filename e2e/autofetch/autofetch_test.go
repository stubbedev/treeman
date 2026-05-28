//go:build e2e

// Package autofetch_e2e exercises the daemon's `auto_fetch:` block
// end-to-end against real git remotes:
//
//   - mode: ff      → a clean worktree fast-forwards to upstream.
//   - mode: rebase  → local commits are replayed on top of upstream.
//   - enabled: false → the periodic sweep's per-repo gate (IsEnabled)
//     reflects the resolved config, while a manual `sync_now` overrides
//     the opt-out and still advances.
//
// auto_fetch is pure-git (no DB engine), so this suite needs only `git`
// on PATH — it skips cleanly when git is absent.
package autofetch_e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
)

// ─── TestAutoFetchFastForwardAdvances ────────────────────────────────
//
// Clean worktree + upstream moved ahead → `mode: ff` fast-forwards HEAD
// to origin/main.
func TestAutoFetchFastForwardAdvances(t *testing.T) {
	requireGit(t)
	isolateConfig(t)
	ctx := context.Background()

	work, origin := makeClone(t)
	advanceOrigin(t, origin)

	originSha := remoteHead(t, work)
	if gitOut(t, work, "rev-parse", "HEAD") == originSha {
		t.Fatal("precondition: clone already at origin/main")
	}

	st := openState(t)
	repoID := mustEnsureRepo(t, st, work)

	cfg := &config.Config{AutoFetch: config.AutoFetchConfig{Mode: "ff"}}
	if err := daemon.SyncRepo(ctx, st, store.RepoRef{ID: repoID, Path: work}, cfg); err != nil {
		t.Fatalf("SyncRepo(ff): %v", err)
	}

	if got := gitOut(t, work, "rev-parse", "HEAD"); got != originSha {
		t.Errorf("HEAD = %s, want %s (origin/main) after ff", got, originSha)
	}
}

// ─── TestAutoFetchRebaseReplaysLocalCommits ──────────────────────────
//
// The behavior untested anywhere before: divergent local commit +
// upstream commit → `mode: rebase` replays the local commit on TOP of
// upstream (origin/main becomes an ancestor of HEAD, both files present).
func TestAutoFetchRebaseReplaysLocalCommits(t *testing.T) {
	requireGit(t)
	isolateConfig(t)
	ctx := context.Background()

	work, origin := makeClone(t)

	// Local commit origin doesn't have.
	writeFile(t, work, "local", "l\n")
	gitRun(t, work, "add", "local")
	gitRun(t, work, "commit", "-q", "-m", "local")

	// Upstream commit the clone doesn't have → divergence.
	advanceOrigin(t, origin)
	originSha := remoteHead(t, work)

	st := openState(t)
	repoID := mustEnsureRepo(t, st, work)

	cfg := &config.Config{AutoFetch: config.AutoFetchConfig{Mode: "rebase"}}
	if err := daemon.SyncRepo(ctx, st, store.RepoRef{ID: repoID, Path: work}, cfg); err != nil {
		t.Fatalf("SyncRepo(rebase): %v", err)
	}

	// Local change survived the replay.
	if _, err := os.Stat(filepath.Join(work, "local")); err != nil {
		t.Errorf("local file missing after rebase: %v", err)
	}
	// Upstream change was pulled in.
	if _, err := os.Stat(filepath.Join(work, "next")); err != nil {
		t.Errorf("upstream file 'next' missing after rebase: %v", err)
	}
	// HEAD is the replayed local commit, not origin's tip…
	if got := gitOut(t, work, "rev-parse", "HEAD"); got == originSha {
		t.Errorf("HEAD == origin/main (%s): local commit was dropped, not replayed", originSha)
	}
	// …and origin/main is now an ancestor (rebase put local on top).
	if err := exec.Command("git", "-C", work, "merge-base", "--is-ancestor", "origin/main", "HEAD").Run(); err != nil {
		t.Errorf("origin/main is not an ancestor of HEAD after rebase: %v", err)
	}
}

// ─── TestAutoFetchDisabledOptOutAndManualOverride ────────────────────
//
// `auto_fetch.enabled: false` is the per-repo opt-out the periodic sweep
// gates on (`runAutoFetchSweep` skips when !IsEnabled()). Assert the
// resolved config reflects the YAML, that the default is on, and that a
// manual `sync_now` overrides the opt-out and still advances.
func TestAutoFetchDisabledOptOutAndManualOverride(t *testing.T) {
	requireGit(t)
	isolateConfig(t)
	ctx := context.Background()

	work, origin := makeClone(t)
	advanceOrigin(t, origin)
	originSha := remoteHead(t, work)

	// Default (no auto_fetch key) → enabled.
	def, err := resolve.LoadResolved(work)
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if !def.AutoFetch.IsEnabled() {
		t.Error("auto_fetch must default to enabled when the key is absent")
	}

	// Opt this repo out via its own .treeman.yaml.
	writeFile(t, work, ".treeman.yaml", "auto_fetch:\n  enabled: false\n")
	off, err := resolve.LoadResolved(work)
	if err != nil {
		t.Fatalf("resolve disabled: %v", err)
	}
	if off.AutoFetch.IsEnabled() {
		t.Error("auto_fetch.enabled: false did not disable the repo (periodic sweep would still pull it)")
	}

	// Manual sync_now is an explicit override — it ignores the opt-out
	// and advances the clean worktree anyway.
	st := openState(t)
	mustEnsureRepo(t, st, work)
	if _, errs := daemon.SyncNow(ctx, st, work); len(errs) != 0 {
		t.Fatalf("SyncNow: %v", errs)
	}
	if got := gitOut(t, work, "rev-parse", "HEAD"); got != originSha {
		t.Errorf("manual sync_now did not advance HEAD: got %s, want %s", got, originSha)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// isolateConfig points XDG_CONFIG_HOME at a throwaway dir so the
// developer's real ~/.config/treeman/config.yaml can't perturb the
// resolved auto_fetch settings under test.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func openState(t *testing.T) *daemon.State {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return daemon.NewState(ctx, s)
}

func mustEnsureRepo(t *testing.T, st *daemon.State, work string) int64 {
	t.Helper()
	id, err := st.Store.EnsureRepo(context.Background(), work, filepath.Base(work))
	if err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	return id
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", full, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", full, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remoteHead returns the sha origin/main currently points at.
func remoteHead(t *testing.T, work string) string {
	t.Helper()
	return strings.Fields(gitOut(t, work, "ls-remote", "origin", "main"))[0]
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// makeClone builds a bare origin (seeded with one commit on main) and a
// working clone tracking origin/main. Returns (workTree, originBare).
func makeClone(t *testing.T) (work, origin string) {
	t.Helper()
	tmp := t.TempDir()
	origin = filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")

	gitRun(t, "", "init", "-q", "-b", "main", "--bare", origin)
	gitRun(t, "", "init", "-q", "-b", "main", seed)
	gitRun(t, seed, "config", "user.email", "t@t")
	gitRun(t, seed, "config", "user.name", "t")
	writeFile(t, seed, "README", "a\n")
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

// advanceOrigin pushes one new commit to origin/main via a throwaway
// clone — simulating another contributor moving upstream forward.
func advanceOrigin(t *testing.T, origin string) {
	t.Helper()
	push := filepath.Join(t.TempDir(), "push")
	gitRun(t, "", "clone", "-q", origin, push)
	gitRun(t, push, "config", "user.email", "t@t")
	gitRun(t, push, "config", "user.name", "t")
	writeFile(t, push, "next", "b\n")
	gitRun(t, push, "add", "next")
	gitRun(t, push, "commit", "-q", "-m", "next")
	gitRun(t, push, "push", "-q", "origin", "main")
}
