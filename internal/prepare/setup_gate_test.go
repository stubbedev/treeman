package prepare

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// Regression for #198: a prepare after a create whose setup never ran
// migrated every database without vendor/ and re-seeded the branch
// database on top of the failure.
func TestRunRefusesWorktreeWhoseSetupFailed(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "treeman.db"))
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	wt := t.TempDir()
	repoID, _ := st.EnsureRepo(ctx, wt, "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, wt, "wtslug", "develop")
	_ = st.WriteEvent(ctx, store.LevelError, store.EvtWorktreeCreateError,
		"create-before-engines: hook environment: timed out", repoID, wtID, store.PhaseBeforeEngines, 0, nil)

	cfg := &config.Config{}
	sl := slug.Slug{Value: "wtslug", Source: slug.SourceTicket}
	_, err = Run(ctx, cfg, wt, sl, st, repoID, wtID, nil)
	if _, ok := errors.AsType[*SetupIncompleteError](err); !ok {
		t.Fatalf("want a *SetupIncompleteError, got %v", err)
	}
	if err := ResetBranchScoped(ctx, cfg, wt, repoID, wtID, st, ""); err == nil {
		t.Error("a reset must not drop branch databases of a worktree whose setup failed")
	}

	_ = st.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateStart, "finalize", repoID, wtID, "", 0, nil)
	if _, err := Run(ctx, cfg, wt, sl, st, repoID, wtID, nil); err != nil {
		t.Errorf("a finalize that re-runs setup must prepare: %v", err)
	}
}
