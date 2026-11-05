package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

func TestSweepInterruptedTeardownsFinishesWhatTheStoppedDaemonLeft(t *testing.T) {
	for _, tc := range []struct {
		name            string
		checkoutTrashed bool
	}{
		{name: "checkout already moved to trash, git admin entry left prunable", checkoutTrashed: true},
		{name: "stopped before the checkout was removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, repo, wt := newWatchWorktreeWithConfig(t, "auto_fetch:\n  enabled: false\n")
			ctx := t.Context()
			if err := FinalizeWorktree(ctx, st, repo, wt, nil, false); err != nil {
				t.Fatal(err)
			}
			row, err := st.Store.LookupActiveWorktreeByPath(ctx, wt)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeDeleteStart,
				"daemon-detached teardown hooks + db teardown + git remove beginning",
				row.RepoID, row.ID, "", 0, nil); err != nil {
				t.Fatal(err)
			}
			if tc.checkoutTrashed {
				trash := filepath.Join(repo, ".worktrees", trashDirName)
				if err := os.MkdirAll(trash, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(wt, filepath.Join(trash, "1-feat")); err != nil {
					t.Fatal(err)
				}
			}

			SweepInterruptedTeardowns(ctx, st)

			assertTeardownFinished(t, st, repo, wt, row)
		})
	}
}

func assertTeardownFinished(t *testing.T, st *State, repo, wt string, row store.WorktreeRow) {
	t.Helper()
	ctx := t.Context()
	pending, err := st.Store.ListInterruptedTeardowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("the sweep must leave no teardown unfinished, still pending: %+v", pending)
	}
	rows, err := st.Store.ListWorktreesForRepo(ctx, row.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == row.ID && !r.Deleted {
			t.Fatalf("the resumed teardown must mark the worktree deleted, got %+v", r)
		}
	}
	evs, err := st.Store.QueryEvents(ctx, store.EventFilter{
		WorktreeID: row.ID,
		EventTypes: []string{store.EvtWorktreeDeleteStart, store.EvtWorktreeDeleteEnd, store.EvtWorktreeDeleteError},
		Limit:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].EventType != store.EvtWorktreeDeleteEnd ||
		evs[1].EventType != store.EvtWorktreeDeleteStart || !strings.Contains(evs[1].PayloadJSON, string(triggerDaemonRestart)) {
		t.Fatalf("the resumed teardown must run as its own daemon_restart-triggered delete and end in delete:end, got %+v", evs)
	}
	if _, err := os.Stat(wt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the checkout must be gone after the resumed teardown, stat err = %v", err)
	}
	if out := gitOut(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, wt) {
		t.Fatalf("git must no longer list the torn-down worktree:\n%s", out)
	}
}
