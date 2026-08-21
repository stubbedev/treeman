package daemon

import (
	"context"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
)

// TestPipelineEnginesTouchedGate pins issue #21: the terminal-error
// defer in FinalizeWorktree only runs recovery drops when engine
// prepare was actually entered. A create-before-engines hook failure
// must leave the flag false so an already-prepared worktree keeps its
// healthy databases.
func TestPipelineEnginesTouchedGate(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()

	repoRoot := t.TempDir()
	wtRoot := t.TempDir()
	repoID, err := st.Store.EnsureRepo(ctx, repoRoot, "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, wtRoot, "slug", "feature")
	if err != nil {
		t.Fatal(err)
	}
	sl := slug.Slug{Value: "slug", Source: slug.SourceTicket}

	// A declared database means prepare would run — the flag must still
	// stay false because the before-engines hook aborts first.
	cfg := config.Config{
		Databases: []config.DatabaseConfig{{Engine: "mysql", NameTemplate: "db_{slug}"}},
		Hooks: config.HooksConfig{
			OnCreateBeforeEngines: []config.Action{{Run: []string{"exit 1"}}},
		},
	}

	touched := false
	if _, err := runFinalizeSetupPipeline(ctx, st, &cfg, repoRoot, wtRoot, sl,
		false, repoID, wtID, nil, false, &touched); err == nil {
		t.Fatal("expected the failing create-before-engines hook to abort the pipeline")
	}
	if touched {
		t.Error("enginesTouched = true after a pre-prepare failure; recovery would drop healthy databases")
	}

	// Same config with a passing hook: prepare is entered (it no-ops on
	// the missing connections block), so the flag flips.
	cfg.Hooks.OnCreateBeforeEngines = []config.Action{{Run: []string{"true"}}}
	touched = false
	if _, err := runFinalizeSetupPipeline(ctx, st, &cfg, repoRoot, wtRoot, sl,
		false, repoID, wtID, nil, false, &touched); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if !touched {
		t.Error("enginesTouched = false after prepare ran; a half-applied prepare would not be recovered")
	}
}

// TestPipelineSkipPrepareGate pins issue #26: skip_prepare must stop
// engine provisioning even when the config has hooks/copies/patches —
// createNeedsWork only honours the flag for a databases-only config, so
// before this gate every throwaway checkout in a repo with hooks still
// got the full engine fanout.
func TestPipelineSkipPrepareGate(t *testing.T) {
	st, cleanup := setup(t)
	defer cleanup()
	ctx := context.Background()

	repoRoot := t.TempDir()
	wtRoot := t.TempDir()
	repoID, err := st.Store.EnsureRepo(ctx, repoRoot, "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, wtRoot, "slug", "feature")
	if err != nil {
		t.Fatal(err)
	}
	sl := slug.Slug{Value: "slug", Source: slug.SourceTicket}

	// Hooks present, so the finalize tail runs regardless; the declared
	// database is what skip_prepare has to suppress.
	cfg := config.Config{
		Databases: []config.DatabaseConfig{{Engine: "mysql", NameTemplate: "db_{slug}"}},
		Hooks: config.HooksConfig{
			OnCreateBeforeEngines: []config.Action{{Run: []string{"true"}}},
		},
	}

	touched := false
	if _, err := runFinalizeSetupPipeline(ctx, st, &cfg, repoRoot, wtRoot, sl,
		false, repoID, wtID, nil, true, &touched); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if touched {
		t.Error("prepare ran under skip_prepare; the throwaway checkout got an engine fanout")
	}
}
