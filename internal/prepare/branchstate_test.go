package prepare

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// fakeNS is an in-memory nsDriver. A namespace is a map of key→value;
// the namespace "exists" iff a map is present for its name. It models
// the four swap primitives so the swap orchestrator can be tested
// without a real engine.
type fakeNS struct {
	data map[string]map[string]string
	// wm is a per-namespace monotonic write counter modelling a real
	// engine's cumulative write stats. Watermark reads it; Restore/Empty
	// and the test write() helper bump it. Capture is a read of the
	// active slot, so it does NOT bump active's counter.
	wm map[string]int
	// wmUnsupported models mongo/redis: no sound cheap watermark, so
	// Watermark returns "" and captures are never skipped.
	wmUnsupported bool
	// captureCalls / restoreCalls count primitive invocations so tests can
	// assert a capture was (or was not) skipped.
	captureCalls int
	restoreCalls int
	// restoreErr, when set, makes Restore fail without mutating — models
	// a crash partway through filling the active namespace.
	restoreErr error
}

func newFakeNS() *fakeNS {
	return &fakeNS{data: map[string]map[string]string{}, wm: map[string]int{}}
}

func clone(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	maps.Copy(out, src)
	return out
}

func (f *fakeNS) Exists(_ context.Context, ns string) (bool, error) {
	_, ok := f.data[ns]
	return ok, nil
}

func (f *fakeNS) Capture(_ context.Context, active, durable string) error {
	f.captureCalls++
	src, ok := f.data[active]
	if !ok {
		return fmt.Errorf("capture: active %q missing", active)
	}
	f.data[durable] = clone(src)
	return nil
}

func (f *fakeNS) Restore(_ context.Context, durable, active string) error {
	f.restoreCalls++
	if f.restoreErr != nil {
		return f.restoreErr
	}
	src, ok := f.data[durable]
	if !ok {
		return fmt.Errorf("restore: durable %q missing", durable)
	}
	f.data[active] = clone(src) // drops active first, then copies
	f.wm[active]++              // a fill is a write to the active slot
	return nil
}

func (f *fakeNS) RestoreParent(_ context.Context, parent, active string, srcKeep func(string) bool) error {
	f.restoreCalls++
	if f.restoreErr != nil {
		return f.restoreErr
	}
	src, ok := f.data[parent]
	if !ok {
		return fmt.Errorf("restore parent: source %q missing", parent)
	}
	dst := map[string]string{}
	for k, v := range src {
		if srcKeep == nil || srcKeep(k) {
			dst[k] = v
		}
	}
	f.data[active] = dst // drops active first, then copies the kept keys
	f.wm[active]++       // a fill is a write to the active slot
	return nil
}

func (f *fakeNS) Empty(_ context.Context, active string) error {
	f.data[active] = map[string]string{}
	f.wm[active]++
	return nil
}

// Watermark returns the per-namespace write counter as an opaque token,
// or "" when watermarks are unsupported (models mongo/redis).
func (f *fakeNS) Watermark(_ context.Context, ns string) (string, error) {
	if f.wmUnsupported {
		return "", nil
	}
	return "wm:" + strconv.Itoa(f.wm[ns]), nil
}

func (f *fakeNS) Drop(_ context.Context, ns string) error {
	delete(f.data, ns)
	return nil
}

func (f *fakeNS) DropDurable(_ context.Context, durable string) error {
	delete(f.data, durable)
	return nil
}

// bsFixture wires a fakeNS + in-memory store into branchScopedArgs for
// a single branch_scoped mysql database keyed off a temp worktree path.
type bsFixture struct {
	t            *testing.T
	st           *store.Store
	fake         *fakeNS
	eng          *branchEngine
	cfg          *config.Config
	d            config.DatabaseConfig
	worktreePath string
	repoID       int64
	worktreeID   int64
	active       string
	parent       func(branch string) (string, bool, error)
	baseBranch   func(branch string) string
	// defaultBranch stands in for origin/HEAD resolution (no git repo in
	// these fixtures). Only read when the worktree row is the main one.
	defaultBranch func() string
	migrateFP     string
}

func newBSFixture(t *testing.T) *bsFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	wtPath := t.TempDir()
	repoID, err := st.EnsureRepo(ctx, wtPath, filepath.Base(wtPath))
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, "wtslug", "develop")
	if err != nil {
		t.Fatal(err)
	}

	d := config.DatabaseConfig{Engine: "mysql", NameTemplate: "app_{slug}", BranchScoped: true}
	cfg := &config.Config{Databases: []config.DatabaseConfig{d}}
	fake := newFakeNS()
	eng := &branchEngine{drv: fake, scope: scopeName, engine: "mysql"}

	active, err := activeNamespace(d, scopeName, wtPath)
	if err != nil {
		t.Fatal(err)
	}
	return &bsFixture{
		t: t, st: st, fake: fake, eng: eng, cfg: cfg, d: d,
		worktreePath: wtPath, repoID: repoID, worktreeID: wtID, active: active,
	}
}

// run points the worktree row at `branch` (simulating a checkout) and
// drives one swap-lifecycle pass.
func (f *bsFixture) run(branch string) Outcome {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.st.EnsureWorktree(ctx, f.repoID, f.worktreePath, "wtslug", branch); err != nil {
		f.t.Fatal(err)
	}
	var rp func(ctx context.Context, branch string) (string, bool, error)
	if f.parent != nil {
		rp = func(_ context.Context, b string) (string, bool, error) { return f.parent(b) }
	}
	var rbb func(ctx context.Context, branch string) string
	if f.baseBranch != nil {
		rbb = func(_ context.Context, b string) string { return f.baseBranch(b) }
	}
	var rdb func(ctx context.Context) string
	if f.defaultBranch != nil {
		rdb = func(context.Context) string { return f.defaultBranch() }
	}
	out, err := runBranchScoped(ctx, branchScopedArgs{
		cfg: f.cfg, d: f.d, dbIdx: 0, worktreePath: f.worktreePath,
		st: f.st, repoID: f.repoID, worktreeID: f.worktreeID,
		eng: f.eng, resolveParent: rp, resolveBaseBranchFn: rbb,
		resolveDefaultBranchFn: rdb, migrateFP: f.migrateFP,
	})
	if err != nil {
		f.t.Fatalf("runBranchScoped(%s): %v", branch, err)
	}
	return out
}

func (f *bsFixture) durable(branch string) string { return f.eng.durable(f.active, branch) }

func (f *bsFixture) set(ns string, kv map[string]string) { f.fake.data[ns] = clone(kv) }

// write models an external (app) write to a namespace: it mutates the
// data AND bumps the watermark, the way a real engine's cumulative write
// counter would advance. Use this — not a bare data-map poke — whenever a
// test needs the swap lifecycle to observe that `active` got dirtied.
func (f *bsFixture) write(ns, k, v string) {
	if f.fake.data[ns] == nil {
		f.fake.data[ns] = map[string]string{}
	}
	f.fake.data[ns][k] = v
	f.fake.wm[ns]++
}

// eventCount returns how many events of `typ` were written for this
// fixture's worktree — used to assert migrate/capture were skipped.
func (f *bsFixture) eventCount(typ string) int {
	f.t.Helper()
	var n int
	err := f.st.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE event_type = ? AND worktree_id = ?`,
		typ, f.worktreeID).Scan(&n)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *bsFixture) assertActive(want ...string) {
	f.t.Helper()
	got, ok := f.fake.data[f.active]
	if !ok {
		f.t.Fatalf("active %q does not exist; want keys %v", f.active, want)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		f.t.Fatalf("active %q keys = %v, want %v", f.active, keys, want)
	}
}

func (f *bsFixture) assertMarker(want string) {
	f.t.Helper()
	got, ok, err := f.st.GetActiveBranch(context.Background(), f.worktreeID, f.active)
	if err != nil {
		f.t.Fatal(err)
	}
	if !ok || got != want {
		f.t.Fatalf("active-branch marker = %q (ok=%t), want %q", got, ok, want)
	}
}

// TestBranchScopedFreshSeedsEmpty: no active, no durable, no parent →
// active is built empty and marked for the branch.
func TestBranchScopedFreshSeedsEmpty(t *testing.T) {
	f := newBSFixture(t)
	f.run("develop")
	f.assertActive() // present, empty
	f.assertMarker("develop")
}

// TestBranchScopedSeedsFromParent: no active, no durable, parent
// resolvable → active filled from the parent namespace.
func TestBranchScopedSeedsFromParent(t *testing.T) {
	f := newBSFixture(t)
	f.set("parentdb", map[string]string{"p": "1"})
	f.parent = func(string) (string, bool, error) { return "parentdb", true, nil }
	f.run("develop")
	f.assertActive("p")
	f.assertMarker("develop")
}

// TestBranchScopedSeedsFromBaseDurable: no active, no own durable, and no
// LIVE base-branch DB to copy — but a durable SNAPSHOT of the base branch
// (develop) exists. The new feature branch must seed from that snapshot
// instead of cold-building an empty schema.
func TestBranchScopedSeedsFromBaseDurable(t *testing.T) {
	f := newBSFixture(t)
	ctx := context.Background()

	// A develop durable snapshot exists for this logical DB, named the way
	// branchEngine.durable derives it for this worktree's active namespace.
	devDur := f.eng.durable(f.active, "develop")
	f.set(devDur, map[string]string{"dev": "1"})
	if err := f.st.RecordBranchDurable(ctx, store.BranchDurableRow{
		RepoID: f.repoID, WorktreeID: f.worktreeID, Engine: "mysql",
		DBKey: f.active, Branch: "develop", DurableName: devDur,
	}); err != nil {
		t.Fatal(err)
	}

	// No LIVE parent DB to copy; base branch resolves to develop.
	f.parent = func(string) (string, bool, error) { return "", false, nil }
	f.baseBranch = func(string) string { return "develop" }

	out := f.run("feature/x")
	if out.Decision != "seed:parent-snapshot" {
		t.Fatalf("decision = %q, want seed:parent-snapshot", out.Decision)
	}
	f.assertActive("dev") // seeded from develop's snapshot
	f.assertMarker("feature/x")
}

// TestBranchScopedSeedsFromDeletedWorktreeDurable: the base branch's
// durable was captured in a worktree that has since been torn down
// (teardown keeps durables). The snapshot seed must still find it via
// the deleted worktree row's stored path.
func TestBranchScopedSeedsFromDeletedWorktreeDurable(t *testing.T) {
	f := newBSFixture(t)
	ctx := context.Background()

	sibPath := t.TempDir()
	sibID, err := f.st.EnsureWorktree(ctx, f.repoID, sibPath, "sibslug", "develop")
	if err != nil {
		t.Fatal(err)
	}
	sibActive, err := activeNamespace(f.d, scopeName, sibPath)
	if err != nil {
		t.Fatal(err)
	}
	sibDur := f.eng.durable(sibActive, "develop")
	f.set(sibDur, map[string]string{"dev": "1"})
	if err := f.st.RecordBranchDurable(ctx, store.BranchDurableRow{
		RepoID: f.repoID, WorktreeID: sibID, Engine: "mysql",
		DBKey: sibActive, Branch: "develop", DurableName: sibDur,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.MarkWorktreeDeleted(ctx, sibID); err != nil {
		t.Fatal(err)
	}

	f.parent = func(string) (string, bool, error) { return "", false, nil }
	f.baseBranch = func(string) string { return "develop" }

	out := f.run("feature/x")
	if out.Decision != "seed:parent-snapshot" {
		t.Fatalf("decision = %q, want seed:parent-snapshot", out.Decision)
	}
	f.assertActive("dev")
}

// TestBranchScopedAdoptsExistingUnmarked: active present, no marker →
// adopt (back it up as the branch's durable, leave data in place).
func TestBranchScopedAdoptsExistingUnmarked(t *testing.T) {
	f := newBSFixture(t)
	f.set(f.active, map[string]string{"k": "v"})
	f.run("develop")
	f.assertActive("k") // untouched
	f.assertMarker("develop")
	if _, ok := f.fake.data[f.durable("develop")]; !ok {
		t.Fatalf("adopt should have captured a durable copy for develop")
	}
}

// TestBranchScopedMainAdoptsUnderDefaultBranch: the main checkout has
// durable copies switched on while it sits on a feature branch. The
// pre-existing data is the shared base dataset, so the first durable copy
// must be labelled with the DEFAULT branch — not the feature branch, whose
// deletion would take the only copy of the base data with it. The feature
// keeps the live active namespace; switching to the default branch restores
// the adopted base data.
func TestBranchScopedMainAdoptsUnderDefaultBranch(t *testing.T) {
	f := newBSFixture(t)
	ctx := context.Background()
	if _, err := f.st.EnsureMainWorktree(ctx, f.repoID, f.worktreePath, "wtslug", "feature/x"); err != nil {
		t.Fatal(err)
	}
	f.defaultBranch = func() string { return "master" }
	f.set(f.active, map[string]string{"base": "1"})

	out := f.run("feature/x")
	if out.Decision != "adopt:master" {
		t.Fatalf("decision = %q, want adopt:master", out.Decision)
	}
	f.assertActive("base") // untouched
	f.assertMarker("feature/x")
	if _, ok := f.fake.data[f.durable("master")]; !ok {
		t.Fatalf("adopt must capture the base data as master's durable copy")
	}
	if _, ok := f.fake.data[f.durable("feature/x")]; ok {
		t.Fatalf("adopt must NOT label the base data with the feature branch")
	}

	// Feature diverges, then the checkout switches to the default branch:
	// feature's work is preserved and master resumes the adopted base data.
	f.write(f.active, "feature", "1")
	out = f.run("master")
	if out.Decision != "swap:resume" {
		t.Fatalf("decision = %q, want swap:resume", out.Decision)
	}
	f.assertActive("base")
	f.assertMarker("master")
	if _, ok := f.fake.data[f.durable("feature/x")]; !ok {
		t.Fatalf("swap must capture feature/x's divergence into its own durable copy")
	}
}

// TestBranchScopedLinkedWorktreeAdoptsUnderItsOwnBranch: the default-branch
// relabel is main-checkout-only. A linked worktree's active namespace holds
// that worktree's own branch data, so adopt keeps labelling it with the
// checked-out branch.
func TestBranchScopedLinkedWorktreeAdoptsUnderItsOwnBranch(t *testing.T) {
	f := newBSFixture(t)
	f.defaultBranch = func() string { return "master" }
	f.set(f.active, map[string]string{"k": "v"})

	out := f.run("feature/x")
	if out.Decision != "adopt" {
		t.Fatalf("decision = %q, want adopt", out.Decision)
	}
	if _, ok := f.fake.data[f.durable("feature/x")]; !ok {
		t.Fatalf("linked worktree adopt must capture durable(feature/x)")
	}
	if _, ok := f.fake.data[f.durable("master")]; ok {
		t.Fatalf("linked worktree adopt must not touch the default branch's durable")
	}
}

// TestBranchScopedRoundTripIsolation is the headline invariant: data
// written on feature stays isolated from develop across a round trip,
// and each branch resumes its own divergent state.
func TestBranchScopedRoundTripIsolation(t *testing.T) {
	f := newBSFixture(t)

	// develop: adopt existing {develop}.
	f.set(f.active, map[string]string{"develop": "1"})
	f.run("develop")
	f.assertActive("develop")

	// switch to new feature → branch point (develop's data), then diverge.
	f.run("feature")
	f.assertActive("develop") // branch point
	f.fake.data[f.active]["feature"] = "1"
	f.assertActive("develop", "feature")

	// back to develop → feature isolated, develop restored.
	f.run("develop")
	f.assertActive("develop")
	f.assertMarker("develop")

	// back to feature → feature's divergence resumed.
	f.run("feature")
	f.assertActive("develop", "feature")
	f.assertMarker("feature")
}

// TestBranchScopedSameBranchNoop: re-running on the same branch leaves
// the active namespace untouched.
func TestBranchScopedSameBranchNoop(t *testing.T) {
	f := newBSFixture(t)
	f.set(f.active, map[string]string{"a": "1"})
	f.run("develop")
	f.fake.data[f.active]["b"] = "2"
	f.run("develop") // noop
	f.assertActive("a", "b")
}

// ─── lever 2: migrate gate ──────────────────────────────────────────

func TestMigrateNeeded(t *testing.T) {
	cases := []struct {
		name       string
		builtEmpty bool
		hasPrev    bool
		prevFP     string
		curFP      string
		want       bool
	}{
		{"fresh empty build always migrates", true, false, "", "fp1", true},
		{"empty build migrates even with stale prev", true, true, "fp1", "fp1", true},
		{"no prior record migrates", false, false, "", "fp1", true},
		{"empty current fingerprint migrates (untrustworthy)", false, true, "fp1", "", true},
		{"changed fingerprint migrates", false, true, "fp1", "fp2", true},
		{"unchanged fingerprint skips", false, true, "fp1", "fp1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := migrateNeeded(c.builtEmpty, c.hasPrev, c.prevFP, c.curFP); got != c.want {
				t.Fatalf("migrateNeeded(%t,%t,%q,%q) = %t, want %t",
					c.builtEmpty, c.hasPrev, c.prevFP, c.curFP, got, c.want)
			}
		})
	}
}

// TestMigrateSkippedOnUnchangedResume drives the full lifecycle with a
// no-op migrate command: the first build migrates and records the
// fingerprint; an unchanged re-run skips (emits migrate_skip); a changed
// fingerprint migrates again.
func TestMigrateSkippedOnUnchangedResume(t *testing.T) {
	f := newBSFixture(t)
	f.d.Migrate = &config.Step{Run: "true"}
	f.migrateFP = "fp-A"

	f.run("develop") // fresh → builtEmpty → migrate runs, records fp-A
	if n := f.eventCount(store.EvtMigrateSkip); n != 0 {
		t.Fatalf("first build must migrate, got %d skips", n)
	}

	f.run("develop") // noop, fp unchanged → skip
	if n := f.eventCount(store.EvtMigrateSkip); n != 1 {
		t.Fatalf("unchanged resume must skip migrate, got %d skips", n)
	}

	f.migrateFP = "fp-B" // a migration landed
	f.run("develop")     // noop but fp changed → migrate runs again
	if n := f.eventCount(store.EvtMigrateSkip); n != 1 {
		t.Fatalf("changed fingerprint must re-migrate (no new skip), got %d skips", n)
	}
}

// TestMigrateGateClearedOnReset: db reset drops the migrated-fingerprint
// so the next prepare re-migrates rather than wrongly skipping.
func TestMigrateGateClearedOnReset(t *testing.T) {
	ctx := context.Background()
	f := newBSFixture(t)
	f.d.Migrate = &config.Step{Run: "true"}
	f.migrateFP = "fp-A"

	f.run("develop")
	if _, ok, _ := f.st.GetBranchMigrated(ctx, f.worktreeID, f.active, "develop"); !ok {
		t.Fatal("first build should record a migrated fingerprint")
	}
	if err := resetActiveNamespace(ctx, f.eng, f.st, f.repoID, f.worktreeID, f.active); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.st.GetBranchMigrated(ctx, f.worktreeID, f.active, "develop"); ok {
		t.Fatal("reset must clear the migrated fingerprint so the next prepare re-migrates")
	}
}

// ─── lever 1: capture gate ──────────────────────────────────────────

func TestCaptureSkippable(t *testing.T) {
	cases := []struct {
		name          string
		prevClean     bool
		durableExists bool
		prevWM, curWM string
		want          bool
	}{
		{"clean + unchanged watermark skips", true, true, "wm:3", "wm:3", true},
		{"dirty never skips", false, true, "wm:3", "wm:3", false},
		{"missing durable never skips", true, false, "wm:3", "wm:3", false},
		{"changed watermark never skips", true, true, "wm:3", "wm:4", false},
		{"empty watermark (unsupported) never skips", true, true, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := captureSkippable(c.prevClean, c.durableExists, c.prevWM, c.curWM); got != c.want {
				t.Fatalf("captureSkippable(%t,%t,%q,%q) = %t, want %t",
					c.prevClean, c.durableExists, c.prevWM, c.curWM, got, c.want)
			}
		})
	}
}

// TestCaptureSkippedOnCleanBounce is the headline lever-1 case: switching
// away from a branch whose active slot is untouched since it was resumed
// skips the (redundant) capture, but a write before the switch forces it.
func TestCaptureSkippedOnCleanBounce(t *testing.T) {
	f := newBSFixture(t)
	f.parent = func(string) (string, bool, error) { return "parentdb", true, nil }
	f.set("parentdb", map[string]string{"base": "1"})

	// Adopt develop: captures a durable copy, marks active clean.
	f.set(f.active, map[string]string{"d": "1"})
	f.run("develop")
	if _, ok := f.fake.data[f.durable("develop")]; !ok {
		t.Fatal("adopt should create durable(develop)")
	}
	c0 := f.fake.captureCalls

	// Switch to feature with NO write to active since the adopt → the
	// capture of develop is redundant and must be skipped.
	f.run("feature")
	if f.fake.captureCalls != c0 {
		t.Fatalf("clean bounce must skip capture: captureCalls %d → %d", c0, f.fake.captureCalls)
	}
	if f.eventCount(store.EvtBranchCaptureSkip) != 1 {
		t.Fatalf("expected one capture_skip event, got %d", f.eventCount(store.EvtBranchCaptureSkip))
	}
	// durable(develop) still holds develop's data — the skip was safe.
	if dd := f.fake.data[f.durable("develop")]; dd["d"] != "1" {
		t.Fatalf("durable(develop) must remain intact after a skipped capture, got %v", dd)
	}

	// Back to develop (resume → clean again), then WRITE before switching.
	f.run("develop")
	c1 := f.fake.captureCalls
	f.write(f.active, "x", "9") // app mutates the working DB
	f.run("feature")
	if f.fake.captureCalls != c1+1 {
		t.Fatalf("a write before switch must force capture: captureCalls %d → %d (want +1)", c1, f.fake.captureCalls)
	}
	if dd := f.fake.data[f.durable("develop")]; dd["x"] != "9" {
		t.Fatalf("forced capture must preserve the new write into durable(develop), got %v", dd)
	}
	// feature's durable copy was created when develop was swapped back in.
	if _, ok := f.fake.data[f.durable("feature")]; !ok {
		t.Fatal("durable(feature) should exist after feature was swapped out")
	}
}

// TestCaptureNeverSkippedWithoutWatermark: an engine with no sound
// watermark (Watermark→"") must always capture, even on an otherwise
// clean bounce — soundness over speed.
func TestCaptureNeverSkippedWithoutWatermark(t *testing.T) {
	f := newBSFixture(t)
	f.fake.wmUnsupported = true

	f.set(f.active, map[string]string{"d": "1"})
	f.run("develop") // adopt
	c0 := f.fake.captureCalls
	f.run("feature") // would-be clean bounce, but no watermark
	if f.fake.captureCalls != c0+1 {
		t.Fatalf("no-watermark engine must always capture: captureCalls %d → %d (want +1)", c0, f.fake.captureCalls)
	}
	if f.eventCount(store.EvtBranchCaptureSkip) != 0 {
		t.Fatalf("no-watermark engine must never emit capture_skip, got %d", f.eventCount(store.EvtBranchCaptureSkip))
	}
}

// TestConnectBranchEngineMissingConn locks the error message for the
// "DB marked branch_scoped but connections.<engine> is unset" path —
// the most common misconfiguration. Every swappable engine must
// surface a clear, engine-specific error before any dial is attempted.
//
// Postgres alias `postgresql` and ES alias `opensearch` are covered
// via engine.Canonical in [TestCanonical] (internal/engine);
// only the canonical-name dispatch is exercised here.
func TestConnectBranchEngineMissingConn(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		engine string
		want   string
	}{
		{"mysql", "connections.mysql not configured"},
		{"postgres", "connections.postgres not configured"},
		{"mongodb", "connections.mongodb not configured"},
		{"redis", "connections.redis not configured"},
		{"elasticsearch", "connections.elasticsearch not configured"},
	}
	for _, c := range cases {
		t.Run(c.engine, func(t *testing.T) {
			eng, closeFn, err := connectBranchEngine(ctx, &config.Config{}, c.engine, nil)
			if err == nil || err.Error() != c.want {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if eng != nil {
				t.Errorf("eng must be nil on error, got %+v", eng)
			}
			// Close must always be safe to invoke, even on the error
			// path — callers defer it unconditionally.
			if closeFn == nil {
				t.Error("closeFn must never be nil; defer-close pattern relies on it")
			} else {
				closeFn()
			}
		})
	}
}

// TestConnectBranchEngineUnswappable: a non-swappable engine (sqlite,
// or any string engine.Canonical rejects) must return (nil, no-op,
// nil) — NOT an error. ResetBranchScoped relies on the nil-eng signal
// to skip such databases silently.
func TestConnectBranchEngineUnswappable(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []string{"sqlite", "", "made-up"} {
		t.Run(engine, func(t *testing.T) {
			eng, closeFn, err := connectBranchEngine(ctx, &config.Config{}, engine, nil)
			if err != nil {
				t.Fatalf("unswappable engine must not error, got %v", err)
			}
			if eng != nil {
				t.Errorf("unswappable engine must return nil branchEngine, got %+v", eng)
			}
			if closeFn == nil {
				t.Error("closeFn must never be nil")
			} else {
				closeFn()
			}
		})
	}
}

// TestResetReseedsFromParent locks the db-reset regression: reset must
// DROP the active namespace (not empty it) so the follow-up prepare
// re-seeds from the live parent. With the old Empty() behaviour the
// follow-up took the adopt branch and left the database empty.
func TestResetReseedsFromParent(t *testing.T) {
	ctx := context.Background()
	f := newBSFixture(t)

	// develop diverged with real data + a durable copy + marker.
	f.set(f.active, map[string]string{"develop": "1"})
	f.run("develop")
	f.assertActive("develop")
	f.assertMarker("develop")

	// reset: drop durable + active, clear marker.
	if err := resetActiveNamespace(ctx, f.eng, f.st, f.repoID, f.worktreeID, f.active); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.fake.data[f.active]; ok {
		t.Fatalf("reset must DROP the active namespace, not leave it present (the adopt-empty bug)")
	}
	if _, ok := f.fake.data[f.durable("develop")]; ok {
		t.Fatalf("reset must drop the current branch's durable copy")
	}
	if _, ok, _ := f.st.GetActiveBranch(ctx, f.worktreeID, f.active); ok {
		t.Fatalf("reset must clear the active-branch marker")
	}

	// follow-up prepare re-seeds from the live parent (NOT empty).
	f.set("parentdb", map[string]string{"base": "1"})
	f.parent = func(string) (string, bool, error) { return "parentdb", true, nil }
	f.run("develop")
	f.assertActive("base")
	f.assertMarker("develop")
}

// TestSaveCapturesWithoutSwitch locks `treeman db save`: capture the
// active namespace into the CURRENT branch's durable copy in place —
// no drop, no marker change — and skip when nothing changed since the
// last capture (same watermark lever as swapBranch).
func TestSaveCapturesWithoutSwitch(t *testing.T) {
	ctx := context.Background()
	f := newBSFixture(t)

	// develop adopted with data; the adopt captured a durable copy.
	f.set(f.active, map[string]string{"develop": "1"})
	f.run("develop")

	// App writes after the adopt — the durable copy is now stale.
	f.write(f.active, "extra", "1")

	out, err := saveActiveNamespace(ctx, f.eng, f.st, f.repoID, f.worktreeID, f.active)
	if err != nil {
		t.Fatal(err)
	}
	if out.Skipped != "" {
		t.Fatalf("save must capture after a write, skipped: %s", out.Skipped)
	}
	if _, ok := f.fake.data[f.durable("develop")]["extra"]; !ok {
		t.Fatalf("durable copy not refreshed: %v", f.fake.data[f.durable("develop")])
	}
	f.assertActive("develop", "extra") // active untouched
	f.assertMarker("develop")          // marker untouched

	// No writes since the save → the second save must skip via watermark.
	before := f.fake.captureCalls
	out2, err := saveActiveNamespace(ctx, f.eng, f.st, f.repoID, f.worktreeID, f.active)
	if err != nil {
		t.Fatal(err)
	}
	if out2.Skipped == "" || f.fake.captureCalls != before {
		t.Fatalf("unchanged save must skip capture (skipped=%q calls=%d→%d)", out2.Skipped, before, f.fake.captureCalls)
	}
}

// TestBranchScopedSwapAdvancesMarkerBeforeFill locks the crash-safety
// ordering: on a branch switch the active-branch marker must advance to
// the NEW branch the moment the OLD branch's data is safe in its durable
// copy — BEFORE fill mutates the active slot. Otherwise a daemon crash
// between fill and the (old) end-of-function marker write would leave the
// marker pointing at the old branch while the active slot holds the new
// branch's data; the next prepare would then re-capture that new data
// into the old branch's durable copy and destroy it.
//
// Here Restore is rigged to fail (a crash partway through fill). With the
// fixed ordering the marker is already "feature" and develop's durable
// copy is intact; the pre-fix ordering would leave the marker at
// "develop".
func TestBranchScopedSwapAdvancesMarkerBeforeFill(t *testing.T) {
	ctx := context.Background()
	f := newBSFixture(t)

	// develop diverged with real data, adopted (durable + marker).
	f.set(f.active, map[string]string{"develop": "1"})
	f.run("develop")

	// Switch to feature, but fill fails partway (simulated crash).
	f.set("parentdb", map[string]string{"base": "1"})
	f.parent = func(string) (string, bool, error) { return "parentdb", true, nil }
	f.fake.restoreErr = errors.New("simulated crash mid-fill")
	if _, err := f.st.EnsureWorktree(ctx, f.repoID, f.worktreePath, "wtslug", "feature"); err != nil {
		t.Fatal(err)
	}
	_, err := runBranchScoped(ctx, branchScopedArgs{
		cfg: f.cfg, d: f.d, dbIdx: 0, worktreePath: f.worktreePath,
		st: f.st, repoID: f.repoID, worktreeID: f.worktreeID, eng: f.eng,
		resolveParent: func(_ context.Context, b string) (string, bool, error) { return f.parent(b) },
	})
	if err == nil {
		t.Fatal("expected the rigged fill failure to surface")
	}

	// Marker advanced to feature before fill (pre-fix: still "develop").
	if got, _, _ := f.st.GetActiveBranch(ctx, f.worktreeID, f.active); got != "feature" {
		t.Fatalf("marker must advance to feature before fill so a re-run can't clobber develop's durable copy; got %q", got)
	}
	// develop's durable copy survived the failed swap intact.
	if dd := f.fake.data[f.durable("develop")]; dd["develop"] != "1" {
		t.Fatalf("durable(develop) must survive a failed swap, got %v", dd)
	}
}
