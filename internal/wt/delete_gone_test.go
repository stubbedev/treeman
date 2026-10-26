package wt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

// A worktree removed by hand (rm -rf, then `git worktree prune`) is what
// `treeman worktree prune` exists to clean up. Daemon-less, the in-process
// teardown re-derived the identity with ResolveIdentity, which refuses a
// path that is no longer a linked worktree, so prune failed on exactly
// that case. The teardown must take the identity from the registry row,
// as the daemon's teardownOrphan does.
func TestInlineDeleteOfWorktreeWhoseDirectoryIsGone(t *testing.T) {
	ctx := context.Background()
	st := withTempStore(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	gitRunT(t, "", "init", "-q", "-b", "main", repo)
	gitRunT(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	marker := filepath.Join(t.TempDir(), "hook-slug")
	cfg := "hooks:\n  delete-before-engines:\n    - run: 'printf %s \"$TREEMAN_SLUG\" > " + marker + "'\n"
	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	wtPath := filepath.Join(repo, ".worktrees", "gone")
	gitRunT(t, repo, "worktree", "add", "-q", "-b", "gone", wtPath)
	repoID, err := st.EnsureRepo(ctx, repo, "repo")
	if err != nil {
		t.Fatal(err)
	}
	const registered = "wt_registered"
	if _, err := st.EnsureWorktree(ctx, repoID, wtPath, registered, "gone"); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatal(err)
	}
	gitRunT(t, repo, "worktree", "prune")

	// What `worktree prune` sends for each registered path that is gone.
	res, err := Delete(ctx, DeleteRequest{RepoRoot: repo, Target: wtPath, Force: true, Detached: true}, NoopSink{})
	if err != nil {
		t.Fatalf("delete of a worktree whose directory is gone: %v", err)
	}
	if res.Status != DeleteInline {
		t.Fatalf("status = %v, want inline", res.Status)
	}
	row, err := st.LookupWorktreeByPath(ctx, wtPath)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Deleted {
		t.Error("registry row not marked deleted")
	}
	evs := inlineLifecycle(t, st, wtPath)
	if len(evs) != 2 || evs[0].EventType != store.EvtWorktreeDeleteStart || evs[1].EventType != store.EvtWorktreeDeleteEnd {
		t.Fatalf("teardown must be bracketed by delete:start and delete:end, got %+v", evs)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("delete-before-engines hook did not run: %v", err)
	}
	if strings.TrimSpace(string(got)) != registered {
		t.Errorf("hook saw TREEMAN_SLUG=%q, want the registry slug %q", got, registered)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree path reappeared: %v", err)
	}

	// A second run finds the row already deleted and is a no-op.
	if _, err := Delete(ctx, DeleteRequest{RepoRoot: repo, Target: wtPath, Force: true, Detached: true}, NoopSink{}); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
