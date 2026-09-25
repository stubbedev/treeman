package prepare

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
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
	finish()
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
	finish2()
}
