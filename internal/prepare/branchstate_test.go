package prepare

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
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
	// restoreErr, when set, makes Restore fail without mutating — models
	// a crash partway through filling the active namespace.
	restoreErr error
}

func newFakeNS() *fakeNS { return &fakeNS{data: map[string]map[string]string{}} }

func clone(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func (f *fakeNS) Exists(_ context.Context, ns string) (bool, error) {
	_, ok := f.data[ns]
	return ok, nil
}
func (f *fakeNS) Capture(_ context.Context, active, durable string) error {
	src, ok := f.data[active]
	if !ok {
		return fmt.Errorf("capture: active %q missing", active)
	}
	f.data[durable] = clone(src)
	return nil
}
func (f *fakeNS) Restore(_ context.Context, durable, active string) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}
	src, ok := f.data[durable]
	if !ok {
		return fmt.Errorf("restore: durable %q missing", durable)
	}
	f.data[active] = clone(src) // drops active first, then copies
	return nil
}
func (f *fakeNS) Empty(_ context.Context, active string) error {
	f.data[active] = map[string]string{}
	return nil
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
	out, err := runBranchScoped(ctx, branchScopedArgs{
		cfg: f.cfg, d: f.d, dbIdx: 0, worktreePath: f.worktreePath,
		st: f.st, repoID: f.repoID, worktreeID: f.worktreeID,
		eng: f.eng, resolveParent: rp,
	})
	if err != nil {
		f.t.Fatalf("runBranchScoped(%s): %v", branch, err)
	}
	return out
}

func (f *bsFixture) durable(branch string) string { return f.eng.durable(f.active, branch) }

func (f *bsFixture) set(ns string, kv map[string]string) { f.fake.data[ns] = clone(kv) }

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
			eng, closeFn, err := connectBranchEngine(ctx, &config.Config{}, c.engine)
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
			eng, closeFn, err := connectBranchEngine(ctx, &config.Config{}, engine)
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
	if err := resetActiveNamespace(ctx, f.eng, f.st, f.worktreeID, f.active); err != nil {
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
	f.fake.restoreErr = fmt.Errorf("simulated crash mid-fill")
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
