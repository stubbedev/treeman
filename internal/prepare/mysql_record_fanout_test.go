package prepare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

type copyCall struct{ op, from, to string }

type fakeMySQLOps struct {
	mu        sync.Mutex
	calls     []copyCall
	failOn    string
	finished  int
	strategy  string
	mergeable bool
}

func (f *fakeMySQLOps) record(op string) cloneRestorer {
	return func(_ context.Context, from, to string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, copyCall{op, from, to})
		if to == f.failOn {
			return errors.New("boom")
		}
		return nil
	}
}

func (f *fakeMySQLOps) ops() incrementalOps {
	return incrementalOps{
		exists:                func(context.Context, string) (bool, error) { return true, nil },
		snapshotRestore:       f.record("restore"),
		snapshotRestoreFanout: f.record("restore-staged"),
		snapshotCreate:        f.record("create"),
		cloneStrategy:         func() string { return f.strategy },
		mergeFanout:           func(context.Context, string) bool { return f.mergeable },
	}
}

func (f *fakeMySQLOps) budget() fanoutBudget {
	return fanoutBudget{progress: func(p CloneProgress) {
		if p != CloneFinished {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.finished++
	}}
}

func (f *fakeMySQLOps) sortedCalls() []copyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.calls)
	slices.SortFunc(out, compareCopyCalls)
	return out
}

func compareCopyCalls(a, b copyCall) int {
	return strings.Compare(a.op+"\x00"+a.from+"\x00"+a.to, b.op+"\x00"+b.from+"\x00"+b.to)
}

type recordFanoutRun struct {
	st     *store.Store
	wtID   int64
	key    snapshot.Key
	ledger *store.AppliedLedger
	clones []string
	err    error
}

func runMySQLRecordFanout(t *testing.T, f *fakeMySQLOps) recordFanoutRun {
	t.Helper()
	st, repoID, wtID := openRollbackStore(t)
	d := config.DatabaseConfig{
		Engine:     "mysql",
		TestClones: &config.TestClonesSpec{Clones: config.ClonesSetting{Fixed: 2}, NameTemplate: "app_test_{n}"},
	}
	inputs := map[string]store.InputVector{"migrations/*.php": {{Path: "migrations/001.php", Hash: "h1"}}}
	r := recordFanoutRun{st: st, wtID: wtID, ledger: store.NewAppliedLedger(inputs), key: snapshot.New("mysql", "8.0", "", "", "", "", nil)}
	r.clones, r.err = snapshotRecordFanout(context.Background(), &config.Config{}, d, template.Context{}, t.TempDir(), st,
		repoID, wtID, "app", r.key.TemplateName(), "8.0", f.budget(), r.key, inputs, r.ledger, f.ops())
	return r
}

func assertRecordedLedger(t *testing.T, r recordFanoutRun) {
	t.Helper()
	rec, err := r.st.LookupSnapshot(context.Background(), r.key.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.TemplateName != r.key.TemplateName() || rec.SourceDB != "app" {
		t.Fatalf("snapshot row = %+v, want template %s of app", rec, r.key.TemplateName())
	}
	got, err := json.Marshal(rec.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(r.ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("recorded ledger = %s, want %s", got, want)
	}
}

func assertWaveEvents(t *testing.T, r recordFanoutRun, merged bool) {
	t.Helper()
	evs, err := r.st.QueryEvents(context.Background(), store.EventFilter{
		WorktreeID: r.wtID,
		EventTypes: []string{store.EvtSnapshotsStrategy, store.EvtClonesStart},
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range evs {
		counts[e.EventType]++
		switch e.EventType {
		case store.EvtSnapshotsStrategy:
			if !strings.Contains(e.Message, "strategy=logical template="+r.key.TemplateName()) {
				t.Errorf("strategy event = %q, want the template's logical strategy", e.Message)
			}
		case store.EvtClonesStart:
			if got := strings.Contains(e.Message, "(template+2 clones)"); got != merged {
				t.Errorf("clones:start = %q, merged = %v, want %v", e.Message, got, merged)
			}
		}
	}
	if counts[store.EvtSnapshotsStrategy] != 1 || counts[store.EvtClonesStart] != 1 {
		t.Errorf("events = %v, want one strategy and one clones:start", counts)
	}
}

// TestMySQLSnapshotRecordFanout: whichever shape the mysql clone strategy
// picks, the template and every clone are built through the clone
// budget, the snapshot row carries the applied ledger and the template's
// clone strategy is reported once. The merged shape builds the template
// alongside the clones straight from the source; the staged shape builds
// the template first and restores the clones from it.
func TestMySQLSnapshotRecordFanout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		merged   bool
		want     func(tpl string) []copyCall
		finished int
	}{
		{
			name:   "merged",
			merged: true,
			want: func(tpl string) []copyCall {
				return []copyCall{{"create", "app", tpl}, {"create", "app", "app_test_1"}, {"create", "app", "app_test_2"}}
			},
			finished: 3,
		},
		{
			name: "staged",
			want: func(tpl string) []copyCall {
				return []copyCall{{"create", "app", tpl}, {"restore-staged", tpl, "app_test_1"}, {"restore-staged", tpl, "app_test_2"}}
			},
			finished: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeMySQLOps{strategy: "logical", mergeable: tc.merged}
			r := runMySQLRecordFanout(t, f)
			if r.err != nil {
				t.Fatal(r.err)
			}
			if !slices.Equal(r.clones, []string{"app_test_1", "app_test_2"}) {
				t.Errorf("clones = %v, want app_test_1 and app_test_2", r.clones)
			}
			want := tc.want(r.key.TemplateName())
			slices.SortFunc(want, compareCopyCalls)
			if got := f.sortedCalls(); !slices.Equal(got, want) {
				t.Errorf("copies = %v, want %v", got, want)
			}
			if f.finished != tc.finished {
				t.Errorf("clone budget reported %d finished restores, want %d", f.finished, tc.finished)
			}
			assertRecordedLedger(t, r)
			assertWaveEvents(t, r, tc.merged)
		})
	}
}

// TestMySQLMergedFanoutFailureRecordsNothing: when a target of the
// merged wave fails, the template is not recorded, so no later prepare
// takes a cache hit on a fan-out that never completed.
func TestMySQLMergedFanoutFailureRecordsNothing(t *testing.T) {
	r := runMySQLRecordFanout(t, &fakeMySQLOps{strategy: "logical", mergeable: true, failOn: "app_test_2"})
	if r.err == nil || !strings.Contains(r.err.Error(), "snapshot create app → app_test_2: boom") {
		t.Fatalf("err = %v, want the failed target's snapshot create", r.err)
	}
	rec, err := r.st.LookupSnapshot(context.Background(), r.key.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Errorf("snapshot row = %+v, want none after a failed merged fan-out", rec)
	}
}

const ancestorMigrations = "migrations/*.php"

// mysqlBuildPath runs one mysql template build path against the fake ops
// with an ancestor template "_tm_ancestor" already restorable into the
// source "app".
type mysqlBuildPath struct {
	name  string
	setup func(t *testing.T, st *store.Store, repoID int64) (config.DatabaseConfig, map[string]store.InputVector)
	run   func(ctx context.Context, d config.DatabaseConfig, st *store.Store, repoID, wtID int64,
		key snapshot.Key, inputs map[string]store.InputVector, f *fakeMySQLOps, worktree string) (Outcome, bool, error)
}

func recordAncestor(
	t *testing.T,
	st *store.Store,
	repoID int64,
	fingerprint string,
	d config.DatabaseConfig,
	inputs map[string]store.InputVector,
) {
	t.Helper()
	if err := st.RecordSnapshot(context.Background(), store.SnapshotRecord{
		Fingerprint:    fingerprint,
		Engine:         "mysql",
		EngineVersion:  "8.0",
		SourceDB:       "app",
		TemplateName:   "_tm_ancestor",
		LockfileHashes: map[string]string{store.CommandsHashKey: commandsHash(d)},
		Inputs:         inputs,
		Ledger:         store.NewAppliedLedger(inputs),
		RepoID:         repoID,
	}); err != nil {
		t.Fatal(err)
	}
}

func mysqlTestClones() *config.TestClonesSpec {
	return &config.TestClonesSpec{Clones: config.ClonesSetting{Fixed: 2}, NameTemplate: "app_test_{n}"}
}

var mysqlBuildPaths = []mysqlBuildPath{
	{
		name: "incremental",
		setup: func(t *testing.T, st *store.Store, repoID int64) (config.DatabaseConfig, map[string]store.InputVector) {
			t.Helper()
			d := config.DatabaseConfig{
				Engine:     "mysql",
				Inputs:     []config.Input{{Glob: ancestorMigrations}},
				Migrate:    &config.Step{Run: "true"},
				TestClones: mysqlTestClones(),
			}
			first := store.InputVector{{Path: "migrations/001.php", Hash: "h1"}}
			recordAncestor(t, st, repoID, "ancestor", d, map[string]store.InputVector{ancestorMigrations: first})
			return d, map[string]store.InputVector{ancestorMigrations: {
				{Path: "migrations/001.php", Hash: "h1"},
				{Path: "migrations/002.php", Hash: "h2"},
			}}
		},
		run: func(ctx context.Context, d config.DatabaseConfig, st *store.Store, repoID, wtID int64,
			key snapshot.Key, inputs map[string]store.InputVector, f *fakeMySQLOps, worktree string,
		) (Outcome, bool, error) {
			return tryIncrementalBuild(ctx, &config.Config{}, d, template.Context{}, worktree, st, repoID, wtID,
				"app", key.TemplateName(), "8.0", f.budget(), key, inputs, nil, time.Now(), f.ops())
		},
	},
	{
		name: "rollback",
		setup: func(t *testing.T, st *store.Store, repoID int64) (config.DatabaseConfig, map[string]store.InputVector) {
			t.Helper()
			const lock = "composer.lock"
			d := config.DatabaseConfig{
				Engine:     "mysql",
				Inputs:     []config.Input{{Glob: ancestorMigrations}, {Glob: lock, Kind: config.InputKindLockfile}},
				Migrate:    &config.Step{Run: "true"},
				TestClones: mysqlTestClones(),
			}
			migs := store.InputVector{{Path: "migrations/001.php", Hash: "h1"}}
			recordAncestor(t, st, repoID, "ancestor", d, map[string]store.InputVector{
				ancestorMigrations: migs, lock: {{Path: lock, Hash: "hOLD"}},
			})
			return d, map[string]store.InputVector{ancestorMigrations: migs, lock: {{Path: lock, Hash: "hNEW"}}}
		},
		run: func(ctx context.Context, d config.DatabaseConfig, st *store.Store, repoID, wtID int64,
			key snapshot.Key, inputs map[string]store.InputVector, f *fakeMySQLOps, worktree string,
		) (Outcome, bool, error) {
			return tryRollbackIncrementalBuild(ctx, &config.Config{}, d, template.Context{}, worktree, st, repoID, wtID,
				"app", key.TemplateName(), "8.0", f.budget(), key, inputs, nil, time.Now(), f.ops())
		},
	},
	{
		name: "dump-only",
		setup: func(t *testing.T, st *store.Store, repoID int64) (config.DatabaseConfig, map[string]store.InputVector) {
			t.Helper()
			d := config.DatabaseConfig{
				Engine:     "mysql",
				Dump:       config.DumpList{{Path: "dump.sql"}},
				Inputs:     []config.Input{{Glob: ancestorMigrations}},
				Migrate:    &config.Step{Run: "true"},
				TestClones: mysqlTestClones(),
			}
			dumpKey := dumpOnlySnapshotKey("mysql", "", "8.0", "")
			recordAncestor(t, st, repoID, dumpKey.Fingerprint(), d, nil)
			return d, map[string]store.InputVector{ancestorMigrations: {{Path: "migrations/001.php", Hash: "h1"}}}
		},
		run: func(ctx context.Context, d config.DatabaseConfig, st *store.Store, repoID, wtID int64,
			key snapshot.Key, inputs map[string]store.InputVector, f *fakeMySQLOps, worktree string,
		) (Outcome, bool, error) {
			return tryDumpOnlyBuild(ctx, &config.Config{}, d, template.Context{}, worktree, st, repoID, wtID,
				"app", key.TemplateName(), "8.0", f.budget(), key, dumpOnlySnapshotKey("mysql", "", "8.0", ""),
				inputs, nil, time.Now(), f.ops())
		},
	},
}

// TestMySQLBuildPathsFanOutFromTheBuiltSource: every mysql build path
// that restores an ancestor into the source and migrates it there
// (incremental, rollback, dump-only) leaves the source equal to the
// template it snapshots, so a logical strategy builds the template and
// the clones from that source in one wave, and a physical one builds the
// template first and restores the clones from it. Either way the
// template is recorded.
func TestMySQLBuildPathsFanOutFromTheBuiltSource(t *testing.T) {
	for _, p := range mysqlBuildPaths {
		for _, merged := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/merged=%v", p.name, merged), func(t *testing.T) {
				ctx := context.Background()
				st, repoID, wtID := openRollbackStore(t)
				d, inputs := p.setup(t, st, repoID)
				key := snapshot.New("mysql", "8.0", "", "", "", "", map[string]string{store.CommandsHashKey: commandsHash(d)})
				f := &fakeMySQLOps{strategy: "logical", mergeable: merged}

				out, done, err := p.run(ctx, d, st, repoID, wtID, key, inputs, f, t.TempDir())
				if err != nil || !done {
					t.Fatalf("done=%v err=%v, want the path to build the template", done, err)
				}
				if !slices.Equal(out.Clones, []string{"app_test_1", "app_test_2"}) {
					t.Errorf("clones = %v, want app_test_1 and app_test_2", out.Clones)
				}

				tpl := key.TemplateName()
				want := []copyCall{{"restore", "_tm_ancestor", "app"}, {"create", "app", tpl}}
				for _, c := range out.Clones {
					if merged {
						want = append(want, copyCall{"create", "app", c})
					} else {
						want = append(want, copyCall{"restore-staged", tpl, c})
					}
				}
				slices.SortFunc(want, compareCopyCalls)
				if got := f.sortedCalls(); !slices.Equal(got, want) {
					t.Errorf("copies = %v, want %v", got, want)
				}

				rec, err := st.LookupSnapshot(ctx, key.Fingerprint())
				if err != nil {
					t.Fatal(err)
				}
				if rec == nil || rec.TemplateName != tpl || rec.Ledger == nil {
					t.Errorf("snapshot row = %+v, want template %s with its ledger", rec, tpl)
				}
				assertWaveEvents(t, recordFanoutRun{st: st, wtID: wtID, key: key}, merged)
			})
		}
	}
}
