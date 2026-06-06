package prepare

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// TestTeardownDatabasesEmitsEventForUnknownEngine confirms the
// regression guard wired into prepare.go: a typo'd
// `engine: postgress` (or any string engine.Canonical rejects)
// teardown path used to fall through silently with `return nil`.
// Now it emits a `db_teardown_skipped` event so a misconfigured
// engine doesn't make worktree delete a no-op without a single
// line in the event log -- same observability principle as the
// cold-build pre-drop event.
func TestTeardownDatabasesEmitsEventForUnknownEngine(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, "/tmp/repo/.worktrees/x", "x", "feature/x")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Databases: []config.DatabaseConfig{
			{Engine: "postgress", NameTemplate: "app_{slug}"}, // typo: extra `s`
		},
	}
	if err := TeardownDatabases(ctx, cfg, "x", repoID, wtID, st); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}

	events, err := st.QueryEvents(ctx, store.EventFilter{
		EventTypes: []string{"db:teardown:skip"},
		WorktreeID: wtID,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("expected at least one db_teardown_skipped event for unknown engine; got none")
	}
	// The message should name the unrecognised engine so a
	// log_query consumer can find the typo.
	if !strings.Contains(events[0].Message, "postgress") {
		t.Errorf("event message %q does not mention the unknown engine name", events[0].Message)
	}
}
