package prepare

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare/progress"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// TestFileColdBuildRecordsLedgerAndFansOutThroughBudget: the file-db
// cold build goes through the shared snapshot/record/fan-out tail
// (#213), so its snapshot row carries the applied ledger, every clone
// restore is reported through the run's progress, and the clones come
// back as resolved paths.
// sqliteWorktree is a worktree with a store row and a data/ directory
// for a sqlite database named data/app_{slug}.db.
type sqliteWorktree struct {
	st           *store.Store
	path         string
	repoID, wtID int64
	source       string
	wtSlug       slug.Slug
}

func newSQLiteWorktree(t *testing.T) sqliteWorktree {
	t.Helper()
	ctx := context.Background()
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "treeman.db"))
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
	if err := os.MkdirAll(filepath.Join(wt, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sqliteWorktree{
		st: st, path: wt, repoID: repoID, wtID: wtID,
		source: filepath.Join(wt, "data", "app_wtslug.db"),
		wtSlug: slug.Slug{Value: "wtslug", Source: slug.SourceTicket},
	}
}

func (w sqliteWorktree) prepare(ctx context.Context, t *testing.T, d config.DatabaseConfig, opts RunOptions) Outcome {
	t.Helper()
	d.Engine, d.NameTemplate = "sqlite", "data/app_{slug}.db"
	outs, err := RunFiltered(
		ctx,
		&config.Config{Databases: []config.DatabaseConfig{d}},
		w.path,
		w.wtSlug,
		w.st,
		w.repoID,
		w.wtID,
		nil,
		opts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].CacheHit {
		t.Fatalf("outcomes = %+v, want one cold build", outs)
	}
	return outs[0]
}

func TestFileColdBuildRecordsLedgerAndFansOutThroughBudget(t *testing.T) {
	ctx := context.Background()
	w := newSQLiteWorktree(t)
	wt := w.path
	if err := os.MkdirAll(filepath.Join(wt, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "migrations", "001_init.sql"), []byte("create table t (id int);"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	reported := map[CloneProgress]int{}
	out := w.prepare(ctx, t, config.DatabaseConfig{
		Inputs:     []config.Input{{Glob: "migrations/*.sql"}},
		Migrate:    &config.Step{Run: "touch " + w.source},
		TestClones: &config.TestClonesSpec{Clones: config.ClonesSetting{Fixed: 2}, NameTemplate: "data/app_{slug}_test_{n}.db"},
	}, RunOptions{Progress: func(p CloneProgress) {
		mu.Lock()
		defer mu.Unlock()
		reported[p]++
	}})

	want := []string{
		filepath.Join(wt, "data", "app_wtslug_test_1.db"),
		filepath.Join(wt, "data", "app_wtslug_test_2.db"),
	}
	if len(out.Clones) != len(want) {
		t.Fatalf("clones = %v, want %v", out.Clones, want)
	}
	for i, c := range out.Clones {
		if c != want[i] {
			t.Errorf("clone %d = %q, want the resolved path %q", i, c, want[i])
		}
		if _, err := os.Stat(c); err != nil {
			t.Errorf("clone %s was not restored: %v", c, err)
		}
	}
	if reported[CloneAdmitted] != len(want) || reported[CloneFinished] != len(want) {
		t.Errorf("clone progress = %v, want %d admitted and finished", reported, len(want))
	}

	rec, err := w.st.LookupSnapshot(ctx, out.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.Ledger == nil {
		t.Fatalf("snapshot %s has no applied ledger: %+v", out.Fingerprint, rec)
	}
	ledger, err := json.Marshal(rec.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var batches map[string]int
	if err := json.Unmarshal(ledger, &batches); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Errorf("ledger = %s, want the one migration in it", ledger)
	}
}

// TestColdBuildReportsPhaseProgress: a cold build reports progress on
// its context as its phases start and finish, so the daemon's stall
// lease is renewed by a build that makes no clone progress (#209).
func TestColdBuildReportsPhaseProgress(t *testing.T) {
	w := newSQLiteWorktree(t)
	var reports atomic.Int32
	ctx := progress.With(context.Background(), func() { reports.Add(1) })
	w.prepare(ctx, t, config.DatabaseConfig{
		Migrate: &config.Step{Run: "touch " + w.source},
		Seed:    &config.Step{Run: "true"},
	}, RunOptions{})
	const want = 6
	if got := reports.Load(); got < want {
		t.Errorf(
			"reported progress %d times, want at least %d (database start, migrate start and end, seed start and end, snapshot, database end)",
			got,
			want,
		)
	}
}
