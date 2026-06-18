package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

// TestSyncNowAdvancesTargetedRepo verifies that SyncNow with a
// specific repo path fetches + advances that repo and emits a
// per-repo status row.
func TestSyncNowAdvancesTargetedRepo(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()
	work, origin := makeClone(t)
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

	rows, errs := SyncNow(ctx, st, work)
	if len(errs) != 0 {
		t.Fatalf("SyncNow errors: %v", errs)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.RepoPath != work {
		t.Errorf("repo_path = %q, want %q", r.RepoPath, work)
	}
	if r.Mode != "ff" {
		t.Errorf("mode = %q, want ff (default)", r.Mode)
	}
	if r.LastFetchUnix == 0 {
		t.Errorf("last_fetch_unix not stamped after successful SyncNow")
	}

	originSha := gitOut(t, work, "ls-remote", "origin", "main")
	wantSha := stripField(originSha)
	gotSha := gitOut(t, work, "rev-parse", "HEAD")
	if gotSha != wantSha {
		t.Errorf("HEAD after sync = %s, want %s", gotSha, wantSha)
	}
}

// TestSyncNowReportsSkipReason verifies that a dirty worktree
// surfaces via SyncWorktreeStatus.LastSkipReason after a sweep.
func TestSyncNowReportsSkipReason(t *testing.T) {
	requireGitAutofetch(t)
	ctx := context.Background()
	work, origin := makeClone(t)
	advanceOrigin(t, origin)

	// Dirty the tree so ff-only refuses.
	if err := writeFile(t, filepath.Join(work, "README"), "dirty\n"); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)
	if _, err := s.EnsureRepo(ctx, work, "work"); err != nil {
		t.Fatal(err)
	}

	rows, errs := SyncNow(ctx, st, work)
	if len(errs) != 0 {
		t.Fatalf("SyncNow errors: %v", errs)
	}
	if len(rows) != 1 || len(rows[0].Worktrees) == 0 {
		t.Fatalf("rows shape: %+v", rows)
	}
	var got string
	for _, w := range rows[0].Worktrees {
		if w.Path == work {
			got = w.LastSkipReason
		}
	}
	if got != SyncSkipDirty {
		t.Errorf("last_skip_reason = %q, want %q", got, SyncSkipDirty)
	}
}

// TestBackoffSchedule sanity-checks the exponential curve hits the
// expected anchors so a future tweak doesn't silently shrink the cap.
func TestBackoffSchedule(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 1 * time.Minute},
		{2, 2 * time.Minute},
		{6, 1 * time.Hour},
		{99, 1 * time.Hour},
	}
	for _, tc := range cases {
		if got := backoffFor(tc.failures); got != tc.want {
			t.Errorf("backoffFor(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}

// TestRepoJitterStable confirms the jitter offset is deterministic
// for a given path (so backoff timing doesn't drift across restarts).
func TestRepoJitterStable(t *testing.T) {
	a := repoJitter("/tmp/foo")
	b := repoJitter("/tmp/foo")
	if a != b {
		t.Errorf("jitter not deterministic: %v vs %v", a, b)
	}
	if a < -30*time.Second || a > 30*time.Second {
		t.Errorf("jitter out of range: %v", a)
	}
}

// stripField returns the first whitespace-delimited token, used to
// pull the sha out of `git ls-remote` output.
func stripField(s string) string {
	for i := range len(s) {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}

func writeFile(t *testing.T, path, body string) error {
	t.Helper()
	return os.WriteFile(path, []byte(body), 0o644)
}
