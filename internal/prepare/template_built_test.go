package prepare

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// TestTemplateBuiltTargets pins the restore-skip gate: a matching
// fingerprint with every target present skips; a missing row, a stale
// fingerprint, or any vanished namespace re-arms the restore.
func TestTemplateBuiltTargets(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/a", "a", "a")

	present := func(context.Context, string) (bool, error) { return true, nil }
	clones := []string{"db_a_test_1", "db_a_test_2"}

	if templateBuiltTargets(ctx, st, present, wtID, "db_a", "mysql", clones, "fp1") {
		t.Fatal("no recorded row: must restore")
	}
	if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "mysql", "fp1"); err != nil {
		t.Fatal(err)
	}
	if !templateBuiltTargets(ctx, st, present, wtID, "db_a", "mysql", clones, "fp1") {
		t.Fatal("recorded fingerprint + all targets present: must skip")
	}
	if templateBuiltTargets(ctx, st, present, wtID, "db_a", "mysql", clones, "fp2") {
		t.Fatal("changed fingerprint: must restore")
	}

	// A vanished namespace re-arms the restore even on a matching
	// fingerprint (manual drop, db reset, engine wipe).
	exists := map[string]bool{}
	probe := func(_ context.Context, name string) (bool, error) { return exists[name], nil }
	exists = map[string]bool{"db_a": true, "db_a_test_1": true} // clone 2 gone
	if templateBuiltTargets(ctx, st, probe, wtID, "db_a", "mysql", clones, "fp1") {
		t.Fatal("missing clone: must restore")
	}
	exists = map[string]bool{"db_a": true, "db_a_test_1": true, "db_a_test_2": true}
	if !templateBuiltTargets(ctx, st, probe, wtID, "db_a", "mysql", clones, "fp1") {
		t.Fatal("all targets back: must skip")
	}
}

// TestCacheHitGenericAlwaysReturnsCallableFinish pins the contract the
// engine functions rely on: `defer finishBuild()` runs unconditionally,
// so cacheHitGeneric must NEVER return a nil func() — a nil compiles
// (untyped nil converts to func()) and instead panics at the deferred
// call, which is what crashed every e2e suite in v2.5.91.
func TestCacheHitGenericAlwaysReturnsCallableFinish(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, "/tmp/repo", "repo")
	wtID, _ := st.EnsureWorktree(ctx, repoID, "/tmp/repo/a", "a", "a")

	exists := func(context.Context, string) (bool, error) { return true, nil }
	restore := func(context.Context, string, string) error { return nil }
	d := config.DatabaseConfig{Engine: "mysql"}
	tplCtx := template.Context{}
	key := snapshot.New("mysql", "8", "", "", "", nil)

	// scenario seeds: which cache state exists before the call.
	for _, scenario := range []string{"no-row", "skip-gate", "full-restore"} {
		t.Run(scenario, func(t *testing.T) {
			_ = st.DeleteSnapshot(ctx, key.Fingerprint())
			_ = st.ClearTemplateBuiltForKey(ctx, wtID, "db_a")
			if scenario == "skip-gate" || scenario == "full-restore" {
				recordSnapshot(ctx, st, key, "mysql", "8", "db_a", key.TemplateName(), nil, repoID)
			}
			// The skip gate needs a recorded built-at fingerprint matching
			// this key AND every target present (exists always true here).
			if scenario == "skip-gate" {
				if err := st.SetTemplateBuilt(ctx, wtID, "db_a", "mysql", key.Fingerprint()); err != nil {
					t.Fatal(err)
				}
			}

			_, _, finish, cerr := cacheHitGeneric(ctx, exists, restore, d, tplCtx, "/wt", st,
				repoID, wtID, "db_a", key, 0, time.Now())
			if cerr != nil {
				t.Fatalf("unexpected error: %v", cerr)
			}
			// finish() must be safe on EVERY outcome — the zero-value
			// flight is a no-op by construction, and the skip-gate path
			// once regressed to a nil func that panicked when deferred.
			finish.finish()
			finish.finish() // idempotent enough for the defer contract
		})
	}
}

// TestBuildFlightsDedupe pins the cross-worktree cold-build gate: a
// concurrent build of the same (engine, fingerprint) is waited for, and
// finish() unblocks the waiter + clears the slot.
func TestBuildFlightsDedupe(t *testing.T) {
	finish := beginBuild("mysql", "fpX")

	var waited sync.WaitGroup
	waited.Add(1)
	var observed bool
	go func() {
		defer waited.Done()
		observed = waitForBuild("mysql", "fpX")
	}()

	// Give the waiter a moment to block on the flight channel.
	time.Sleep(20 * time.Millisecond)
	finish.finish()
	waited.Wait()

	if !observed {
		t.Fatal("in-flight build must be observed")
	}
	// Slot cleared: a later waiter reports nothing in flight.
	if waitForBuild("mysql", "fpX") {
		t.Fatal("finished build must clear its slot")
	}
	// Different fingerprints never block on each other.
	finish2 := beginBuild("mysql", "fpY")
	if waitForBuild("mysql", "fpZ") || waitForBuild("postgres", "fpX") {
		t.Fatal("unrelated flights must not be waited on")
	}
	// The zero flight (no cold build in progress) is a safe no-op.
	buildFlight{}.finish()
	finish2.finish()
}
