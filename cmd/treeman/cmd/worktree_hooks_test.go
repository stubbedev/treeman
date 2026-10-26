package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
)

func TestWorktreeShowNonFatalHookFailure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/wt", "slug", "feature")
	_, err = st.WriteHookRun(ctx, wtID, "create-before-engines", 0, "npm ci", 1000, 1500, 7, "", "lockfile invalid")
	if err != nil {
		t.Fatal(err)
	}
	message := "setup + prepare complete with 1 non-fatal hook failure(s): create-before-engines: hook exited 7: npm ci"
	if err := st.WriteEvent(ctx, store.LevelWarn, store.EvtWorktreeCreateEnd, message, repoID, wtID, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	line, err := finalizeStatusLine(ctx, st, wtID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ready (warnings)", "non-fatal", "npm ci", "exited 7"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %s", want, line)
		}
	}
	runs, err := st.QueryHookRuns(ctx, wtID, 5)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	old := ui.Out
	ui.Out = &buf
	defer func() { ui.Out = old }()
	printWtHookRuns(runs)
	for _, want := range []string{"recent hook runs", "create-before-engines", "7"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("missing %q in %s", want, buf.String())
		}
	}
	if err := st.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateEnd, "complete", repoID, wtID, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	if line, err := finalizeStatusLine(ctx, st, wtID); err != nil || strings.Contains(line, "warnings") {
		t.Fatalf("stale warning after retry: %s (err %v)", line, err)
	}
}

func TestWorktreeShowFailedFinalizeNamesTheRetryCommand(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/wt", "slug", "feature")
	if err := st.WriteEvent(ctx, store.LevelError, store.EvtWorktreeCreateError,
		"create-before-engines: hook exited 4", repoID, wtID, "", 0, nil); err != nil {
		t.Fatal(err)
	}
	line, err := finalizeStatusLine(ctx, st, wtID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"error", "hook exited 4", "treeman worktree finalize"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %s", want, line)
		}
	}
}

func TestWorktreeStateReadsSurfaceQueryErrors(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/wt", "slug", "feature")
	if _, err := st.DB.ExecContext(ctx, "DROP TABLE events"); err != nil {
		t.Fatal(err)
	}
	if line, err := finalizeStatusLine(ctx, st, wtID); err == nil || !strings.Contains(err.Error(), "worktree #") {
		t.Errorf("worktree show: a failed state query must fail naming the worktree, got %q / %v", line, err)
	}
	rows := []wtRow{{ID: wtID, Path: "/tmp/repo/wt"}}
	if err := enrichWtRows(ctx, st, rows, false, true, false, false); err == nil {
		t.Errorf("worktree list --with-state: a failed state query must fail, got state %q", rows[0].FinalState)
	}
}
