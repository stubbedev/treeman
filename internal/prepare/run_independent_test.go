package prepare

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// TestRunFilteredFailureDoesNotCancelSiblings: one database failing its
// migrate must not cut a sibling's prepare short (#119). The slow sibling
// still finishes its migrate, and the error names only the failed index.
func TestRunFilteredFailureDoesNotCancelSiblings(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	wt := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, wt, "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, wt, "wtslug", "develop")
	if err != nil {
		t.Fatal(err)
	}
	done := filepath.Join(wt, "slow.done")
	slowDB := filepath.Join(wt, "data", "slow_wtslug.db")
	if err := os.MkdirAll(filepath.Dir(slowDB), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Databases: []config.DatabaseConfig{
		{Engine: "sqlite", NameTemplate: "data/fail_{slug}.db", Migrate: &config.Step{Run: "exit 1"}},
		{Engine: "sqlite", NameTemplate: "data/slow_{slug}.db", Migrate: &config.Step{Run: "sleep 0.5 && touch " + slowDB + " " + done}},
	}}

	outs, err := Run(ctx, cfg, wt, slug.Slug{Value: "wtslug", Source: slug.SourceTicket}, st, repoID, wtID, nil)
	if err == nil {
		t.Fatal("want the failing database's error")
	}
	if got := RecoverDBIndices(err); !slices.Equal(got, []int{0}) {
		t.Fatalf("RecoverDBIndices = %v, want [0] (err: %v)", got, err)
	}
	if _, serr := os.Stat(done); serr != nil {
		t.Fatalf("sibling migrate was cut short: %v", serr)
	}
	if len(outs) != 1 {
		t.Fatalf("want the sibling's outcome, got %d outcomes", len(outs))
	}
}
