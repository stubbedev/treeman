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

// TestPrintWtPrepareSummary seeds prepare:end + prepare:phase events
// and asserts the "last prepare" block renders the newest run per
// database — outcome, duration, clone count, and the per-phase
// breakdown — without leaking older runs.
func TestPrintWtPrepareSummary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/wt", "feature-x", "feature-x")

	// Older run (cold build) then newer run (cache hit) for the same DB
	// — only the newer must render.
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd, "old",
		repoID, wtID, "", 0, map[string]string{
			"engine": "postgres", "source_db": "app_x",
			"cache_hit": "false", "duration_ms": "9000", "clones": "2",
		})
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPreparePhase, "phase",
		repoID, wtID, "migrate", 1200, map[string]string{
			"engine": "postgres", "source_db": "app_x", "duration_ms": "1200",
		})
	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtPrepareEnd, "new",
		repoID, wtID, "", 0, map[string]string{
			"engine": "postgres", "source_db": "app_x",
			"cache_hit": "true", "duration_ms": "412", "clones": "2",
		})

	var buf bytes.Buffer
	oldOut := ui.Out
	ui.Out = &buf
	defer func() { ui.Out = oldOut }()

	printWtPrepareSummary(ctx, st, wtID)
	out := buf.String()
	for _, want := range []string{"last prepare", "postgres", "app_x", "cache hit", "412ms", "2 clones", "migrate 1.2s"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "9s") {
		t.Errorf("older run leaked into summary:\n%s", out)
	}
}
