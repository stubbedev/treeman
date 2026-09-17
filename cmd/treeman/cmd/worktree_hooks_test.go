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
	line := finalizeStatusLine(ctx, st, wtID)
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
	if line := finalizeStatusLine(ctx, st, wtID); strings.Contains(line, "warnings") {
		t.Fatalf("stale warning after retry: %s", line)
	}
}
