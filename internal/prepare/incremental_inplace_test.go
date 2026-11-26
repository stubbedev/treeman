package prepare

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

const migratedLog = "migrated.log"

// inPlaceRun drives tryIncrementalBuild for a mysql database "app" with
// clones app_test_1 and app_test_2 against fake copy ops whose write
// watermarks read from watermarks. Its migrate step appends the target
// it ran against to migrated.log in the worktree.
type inPlaceRun struct {
	t          *testing.T
	st         *store.Store
	repoID     int64
	wtID       int64
	worktree   string
	d          config.DatabaseConfig
	watermarks map[string]string
}

// newInPlaceRun sets one up whose migrate step fails for the target
// failMigrateOn ("" for none).
func newInPlaceRun(t *testing.T, failMigrateOn string) *inPlaceRun {
	t.Helper()
	st, repoID, wtID := openRollbackStore(t)
	run := `printf '%s\n' "$TREEMAN_TARGET_DB" >> ` + migratedLog
	if failMigrateOn != "" {
		run += ` && [ "$TREEMAN_TARGET_DB" != "` + failMigrateOn + `" ]`
	}
	d := config.DatabaseConfig{
		Engine:     "mysql",
		Inputs:     []config.Input{{Glob: ancestorMigrations}},
		Migrate:    &config.Step{Run: run},
		TestClones: mysqlTestClones(),
	}
	recordAncestor(t, st, repoID, "ancestor", d, map[string]store.InputVector{ancestorMigrations: migrations(1)})
	return &inPlaceRun{
		t: t, st: st, repoID: repoID, wtID: wtID, worktree: t.TempDir(), d: d,
		watermarks: map[string]string{"app": "ps:app:10", "app_test_1": "ps:app_test_1:20", "app_test_2": "ps:app_test_2:30"},
	}
}

func migrations(n int) store.InputVector {
	v := make(store.InputVector, 0, n)
	for i := 1; i <= n; i++ {
		v = append(v, store.FileHash{Path: fmt.Sprintf("migrations/%03d.php", i), Hash: fmt.Sprintf("h%d", i)})
	}
	return v
}

// recordBuilt records what Run records after a build: the targets were
// left at fingerprint with the given watermarks.
func (r *inPlaceRun) recordBuilt(fingerprint string, watermarks map[string]string) {
	r.t.Helper()
	if err := recordTemplateBuilt(context.Background(), r.st, r.wtID, "app", "mysql", "",
		store.TemplateBuilt{Fingerprint: fingerprint, Watermarks: watermarks}); err != nil {
		r.t.Fatal(err)
	}
}

// build runs one incremental build with the first migrationCount
// migrations as its inputs.
func (r *inPlaceRun) build(f *fakeMySQLOps, migrationCount int) (Outcome, snapshot.Key) {
	r.t.Helper()
	ops := f.ops()
	ops.watermark = func(_ context.Context, ns string) (string, error) { return r.watermarks[ns], nil }
	key := snapshot.New("mysql", "8.0", "", "", fmt.Sprintf("m%d", migrationCount), "",
		map[string]string{store.CommandsHashKey: commandsHash(r.d)})
	inputs := map[string]store.InputVector{ancestorMigrations: migrations(migrationCount)}
	out, done, err := tryIncrementalBuild(context.Background(), &config.Config{}, r.d, template.Context{}, r.worktree,
		r.st, r.repoID, r.wtID, "app", key.TemplateName(), "8.0", f.budget(), key, inputs, nil, time.Now(), ops)
	if err != nil || !done {
		r.t.Fatalf("done=%v err=%v, want the incremental build to succeed", done, err)
	}
	return out, key
}

// migrated returns the targets migrate ran against since the last call.
func (r *inPlaceRun) migrated() []string {
	r.t.Helper()
	path := filepath.Join(r.worktree, migratedLog)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		r.t.Fatal(err)
	}
	got := strings.Fields(string(raw))
	slices.Sort(got)
	return got
}

// paths returns the path each target took per prepare:incremental:target
// event, keyed "db=path"; the restore taken after a failed in-place
// migrate is reported as "db=restore(after in-place)".
func (r *inPlaceRun) paths() []string {
	r.t.Helper()
	evs, err := r.st.QueryEvents(context.Background(), store.EventFilter{
		WorktreeID: r.wtID, EventTypes: []string{store.EvtPrepareIncrementalTarget},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		var p map[string]string
		if err := json.Unmarshal([]byte(e.PayloadJSON), &p); err != nil {
			r.t.Fatal(err)
		}
		entry := p["db"] + "=" + p["path"]
		if e.Level == store.LevelWarn {
			entry += "(after in-place)"
		}
		out = append(out, entry)
	}
	slices.Sort(out)
	return out
}

func assertCopies(t *testing.T, f *fakeMySQLOps, want []copyCall) {
	t.Helper()
	slices.SortFunc(want, compareCopyCalls)
	if got := f.sortedCalls(); !slices.Equal(got, want) {
		t.Errorf("copies = %v, want %v", got, want)
	}
}

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// TestIncrementalMigratesUntouchedTargetsInPlace: when the previous build
// left the source and every clone at the ancestor and none was written
// since, the source skips the ancestor restore and every clone is
// migrated in place; only the template is copied from the migrated
// source. The build forgets the old record before writing, and reports
// the watermarks Run records for the next build.
func TestIncrementalMigratesUntouchedTargetsInPlace(t *testing.T) {
	r := newInPlaceRun(t, "")
	r.recordBuilt("ancestor", maps.Clone(r.watermarks))
	f := &fakeMySQLOps{strategy: "logical"}

	out, key := r.build(f, 2)

	assertCopies(t, f, []copyCall{{"create", "app", key.TemplateName()}})
	assertStrings(t, "migrated", r.migrated(), []string{"app", "app_test_1", "app_test_2"})
	assertStrings(t, "paths", r.paths(), []string{"app=in-place", "app_test_1=in-place", "app_test_2=in-place"})
	if !maps.Equal(out.watermarks, r.watermarks) {
		t.Errorf("outcome watermarks = %v, want %v", out.watermarks, r.watermarks)
	}
	if _, ok, err := r.st.GetTemplateBuilt(context.Background(), r.wtID, "app", "mysql", ""); err != nil || ok {
		t.Errorf("built-at record after the build: ok=%v err=%v, want it forgotten until Run records the new one", ok, err)
	}
}

// TestIncrementalRecordedStateAdvances: once Run records what an in-place
// build reported, the next appended migration finds the targets at the
// new fingerprint and migrates them in place again.
func TestIncrementalRecordedStateAdvances(t *testing.T) {
	r := newInPlaceRun(t, "")
	r.recordBuilt("ancestor", maps.Clone(r.watermarks))
	first, firstKey := r.build(&fakeMySQLOps{strategy: "logical"}, 2)
	r.recordBuilt(first.Fingerprint, first.watermarks)
	_ = r.migrated()

	f := &fakeMySQLOps{strategy: "logical"}
	second, secondKey := r.build(f, 3)

	if second.IncrementalBase != firstKey.Fingerprint() {
		t.Fatalf("second build's ancestor = %s, want the first build's %s", second.IncrementalBase, firstKey.Fingerprint())
	}
	assertCopies(t, f, []copyCall{{"create", "app", secondKey.TemplateName()}})
	assertStrings(t, "migrated", r.migrated(), []string{"app", "app_test_1", "app_test_2"})
}

// TestIncrementalRestoresAWrittenSource: a source written to since its
// build is restored from the ancestor before it migrates, while the
// untouched clones are still migrated in place.
func TestIncrementalRestoresAWrittenSource(t *testing.T) {
	r := newInPlaceRun(t, "")
	r.recordBuilt("ancestor", maps.Clone(r.watermarks))
	r.watermarks["app"] = "ps:app:11"
	f := &fakeMySQLOps{strategy: "logical"}

	_, key := r.build(f, 2)

	assertCopies(t, f, []copyCall{{"restore", "_tm_ancestor", "app"}, {"create", "app", key.TemplateName()}})
	assertStrings(t, "migrated", r.migrated(), []string{"app", "app_test_1", "app_test_2"})
	assertStrings(t, "paths", r.paths(), []string{"app=restore", "app_test_1=in-place", "app_test_2=in-place"})
}

// TestIncrementalRecopiesDirtyClones: a clone written to since its build
// is re-copied from the new template (or, merged, from the migrated
// source) while its untouched peer is migrated in place.
func TestIncrementalRecopiesDirtyClones(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "staged", true: "merged"}[merged], func(t *testing.T) {
			r := newInPlaceRun(t, "")
			r.recordBuilt("ancestor", maps.Clone(r.watermarks))
			r.watermarks["app_test_2"] = "ps:app_test_2:31"
			f := &fakeMySQLOps{strategy: "logical", mergeable: merged}

			_, key := r.build(f, 2)

			tpl := key.TemplateName()
			want := []copyCall{{"create", "app", tpl}, {"restore-staged", tpl, "app_test_2"}}
			if merged {
				want[1] = copyCall{"create", "app", "app_test_2"}
			}
			assertCopies(t, f, want)
			assertStrings(t, "migrated", r.migrated(), []string{"app", "app_test_1"})
			assertStrings(t, "paths", r.paths(), []string{"app=in-place", "app_test_1=in-place", "app_test_2=restore"})
		})
	}
}

// TestIncrementalRestoresEverythingWithoutProof: no record, a record of
// another fingerprint, a record without watermarks or watermarks the
// engine no longer reports all leave every target re-copied, as before
// #229.
func TestIncrementalRestoresEverythingWithoutProof(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(r *inPlaceRun)
	}{
		{name: "no record", setup: func(*inPlaceRun) {}},
		{name: "other fingerprint", setup: func(r *inPlaceRun) { r.recordBuilt("elsewhere", maps.Clone(r.watermarks)) }},
		{name: "no watermarks", setup: func(r *inPlaceRun) { r.recordBuilt("ancestor", nil) }},
		{name: "watermarks unreadable now", setup: func(r *inPlaceRun) {
			r.recordBuilt("ancestor", maps.Clone(r.watermarks))
			r.watermarks = map[string]string{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newInPlaceRun(t, "")
			tc.setup(r)
			f := &fakeMySQLOps{strategy: "logical"}

			_, key := r.build(f, 2)

			tpl := key.TemplateName()
			assertCopies(t, f, []copyCall{
				{"restore", "_tm_ancestor", "app"},
				{"create", "app", tpl},
				{"restore-staged", tpl, "app_test_1"},
				{"restore-staged", tpl, "app_test_2"},
			})
			assertStrings(t, "migrated", r.migrated(), []string{"app"})
			assertStrings(t, "paths", r.paths(), []string{"app=restore", "app_test_1=restore", "app_test_2=restore"})
		})
	}
}

// TestIncrementalRecopiesACloneWhoseInPlaceMigrateFails: a clone whose
// in-place migrate fails is restored from the new template instead, and
// the build still succeeds.
func TestIncrementalRecopiesACloneWhoseInPlaceMigrateFails(t *testing.T) {
	r := newInPlaceRun(t, "app_test_1")
	r.recordBuilt("ancestor", maps.Clone(r.watermarks))
	f := &fakeMySQLOps{strategy: "logical"}

	out, key := r.build(f, 2)

	tpl := key.TemplateName()
	assertCopies(t, f, []copyCall{{"create", "app", tpl}, {"restore-staged", tpl, "app_test_1"}})
	assertStrings(t, "migrated", r.migrated(), []string{"app", "app_test_1", "app_test_2"})
	assertStrings(t, "paths", r.paths(), []string{
		"app=in-place", "app_test_1=in-place", "app_test_1=restore(after in-place)", "app_test_2=in-place",
	})
	if !slices.Equal(out.Clones, []string{"app_test_1", "app_test_2"}) {
		t.Errorf("clones = %v, want app_test_1 and app_test_2", out.Clones)
	}
}
