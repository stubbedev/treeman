package prepare

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

func openRollbackStore(t *testing.T) (*store.Store, int64, int64) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repoID, err := st.EnsureRepo(ctx, "/r", "r")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, "/r/wt", "wt", "main")
	if err != nil {
		t.Fatal(err)
	}
	return st, repoID, wtID
}

func TestRollbackFallbackReportsTheFailingStepsOutput(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openRollbackStore(t)

	d := config.DatabaseConfig{
		Engine:   "mysql",
		Migrate:  &config.Step{Run: "echo 'Column not found: dead_column' >&2; exit 1"},
		Rollback: &config.Step{Run: "true"},
	}
	key := snapshot.New("mysql", "8.0", "", "", "", "", map[string]string{store.CommandsHashKey: commandsHash(d)})
	const glob = "migrations/*.php"
	ancInputs := map[string]store.InputVector{glob: {
		{Path: "migrations/001.php", Hash: "h1"},
		{Path: "migrations/002.php", Hash: "hOLD"},
	}}
	if err := st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    "ancestor",
		Engine:         "mysql",
		EngineVersion:  "8.0",
		SourceDB:       "src",
		TemplateName:   "_tm_ancestor",
		LockfileHashes: map[string]string{store.CommandsHashKey: commandsHash(d)},
		Inputs:         ancInputs,
		Ledger:         store.NewAppliedLedger(ancInputs),
		RepoID:         repoID,
	}); err != nil {
		t.Fatal(err)
	}
	current := map[string]store.InputVector{glob: {
		{Path: "migrations/001.php", Hash: "h1"},
		{Path: "migrations/002.php", Hash: "hNEW"},
	}}
	ops := incrementalOps{
		exists:          func(context.Context, string) (bool, error) { return true, nil },
		snapshotRestore: func(context.Context, string, string) error { return nil },
	}

	_, done, err := tryRollbackIncrementalBuild(ctx, &config.Config{}, d, template.Context{}, t.TempDir(), st,
		repoID, wtID, "src", key.TemplateName(), "8.0", fanoutBudget{}, key, current, nil, time.Now(), ops)
	if err != nil || done {
		t.Fatalf("a failed re-migrate must fall through to the next build path: done=%v err=%v", done, err)
	}

	evs, err := st.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtPrepareRollbackFallback}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("rollback fallback events = %d, want 1", len(evs))
	}
	msg := evs[0].Message
	if !strings.Contains(msg, "Column not found: dead_column") || strings.Contains(msg, "<nil>") {
		t.Errorf("fallback reason must carry the migrate stderr, got %q", msg)
	}

	errEvs, err := st.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtPrepareError}})
	if err != nil {
		t.Fatal(err)
	}
	if len(errEvs) != 0 {
		t.Errorf("a recovered rollback step must not emit a terminal prepare:error, got %+v", errEvs)
	}
}

func TestLockfileOnlyChangeMigratesForwardWithoutRollback(t *testing.T) {
	for name, rollback := range map[string]*config.Step{
		"rollback configured": {Run: "echo ROLLBACK_RAN >&2; exit 1"},
		"no rollback block":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st, repoID, wtID := openRollbackStore(t)

			const migs, lock = "migrations/*.php", "composer.lock"
			d := config.DatabaseConfig{
				Engine:   "mysql",
				Inputs:   []config.Input{{Glob: migs}, {Glob: lock, Kind: config.InputKindLockfile}},
				Migrate:  &config.Step{Run: "echo MIGRATE_RAN >&2; exit 1"},
				Rollback: rollback,
			}
			key := snapshot.New("mysql", "8.0", "", "", "", "", map[string]string{store.CommandsHashKey: commandsHash(d)})
			ledger := store.InputVector{{Path: "migrations/001.php", Hash: "h1"}}
			ancInputs := map[string]store.InputVector{
				migs: ledger,
				lock: {{Path: "composer.lock", Hash: "hOLD"}},
			}
			if err := st.RecordSnapshot(ctx, store.SnapshotRecord{
				Fingerprint:    "ancestor",
				Engine:         "mysql",
				EngineVersion:  "8.0",
				SourceDB:       "src",
				TemplateName:   "_tm_ancestor",
				LockfileHashes: map[string]string{store.CommandsHashKey: commandsHash(d)},
				Inputs:         ancInputs,
				Ledger:         store.NewAppliedLedger(ancInputs),
				RepoID:         repoID,
			}); err != nil {
				t.Fatal(err)
			}
			current := map[string]store.InputVector{
				migs: ledger,
				lock: {{Path: "composer.lock", Hash: "hNEW"}},
			}
			var restored string
			ops := incrementalOps{
				exists: func(context.Context, string) (bool, error) { return true, nil },
				snapshotRestore: func(_ context.Context, tmpl, _ string) error {
					restored = tmpl
					return nil
				},
			}

			_, done, err := tryRollbackIncrementalBuild(ctx, &config.Config{}, d, template.Context{}, t.TempDir(), st,
				repoID, wtID, "src", key.TemplateName(), "8.0", fanoutBudget{}, key, current, nil, time.Now(), ops)
			if err != nil || done {
				t.Fatalf("the failing migrate must fall through: done=%v err=%v", done, err)
			}
			if restored != "_tm_ancestor" {
				t.Errorf("ancestor restored = %q, want _tm_ancestor", restored)
			}

			evs, err := st.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtPrepareRollbackFallback}})
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != 1 {
				t.Fatalf("fallback events = %d, want 1", len(evs))
			}
			msg := evs[0].Message
			if !strings.Contains(msg, "MIGRATE_RAN") || strings.Contains(msg, "ROLLBACK_RAN") {
				t.Errorf("a lockfile-only change must skip rollback and migrate forward, got %q", msg)
			}
		})
	}
}

func TestInsertedEarlierMigrationsMigrateForwardWithoutRollback(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openRollbackStore(t)

	const migs = "migrations/*.php"
	d := config.DatabaseConfig{
		Engine:   "mysql",
		Inputs:   []config.Input{{Glob: migs}},
		Migrate:  &config.Step{Run: "echo MIGRATE_RAN >&2; exit 1"},
		Rollback: &config.Step{Run: "echo ROLLBACK_RAN >&2; exit 1"},
	}
	key := snapshot.New("mysql", "8.0", "", "", "", "", map[string]string{store.CommandsHashKey: commandsHash(d)})
	ancInputs := map[string]store.InputVector{migs: {
		{Path: "migrations/2026_09_01_base.php", Hash: "h1"},
		{Path: "migrations/2026_10_14_branch_drop_columns.php", Hash: "hb"},
	}}
	if err := st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    "ancestor",
		Engine:         "mysql",
		EngineVersion:  "8.0",
		SourceDB:       "src",
		TemplateName:   "_tm_ancestor",
		LockfileHashes: map[string]string{store.CommandsHashKey: commandsHash(d)},
		Inputs:         ancInputs,
		Ledger:         store.NewAppliedLedger(ancInputs),
		RepoID:         repoID,
	}); err != nil {
		t.Fatal(err)
	}
	current := map[string]store.InputVector{migs: {
		{Path: "migrations/2026_09_01_base.php", Hash: "h1"},
		{Path: "migrations/2026_10_01_develop.php", Hash: "hd"},
		{Path: "migrations/2026_10_14_branch_drop_columns.php", Hash: "hb"},
	}}
	var restored string
	ops := incrementalOps{
		exists: func(context.Context, string) (bool, error) { return true, nil },
		snapshotRestore: func(_ context.Context, tmpl, _ string) error {
			restored = tmpl
			return nil
		},
	}

	_, done, err := tryRollbackIncrementalBuild(ctx, &config.Config{}, d, template.Context{}, t.TempDir(), st,
		repoID, wtID, "src", key.TemplateName(), "8.0", fanoutBudget{}, key, current, nil, time.Now(), ops)
	if err != nil || done {
		t.Fatalf("the failing migrate must fall through: done=%v err=%v", done, err)
	}
	if restored != "_tm_ancestor" {
		t.Errorf("ancestor restored = %q, want _tm_ancestor", restored)
	}

	evs, err := st.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, EventTypes: []string{store.EvtPrepareRollbackFallback}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("fallback events = %d, want 1", len(evs))
	}
	msg := evs[0].Message
	if !strings.Contains(msg, "MIGRATE_RAN") || strings.Contains(msg, "ROLLBACK_RAN") {
		t.Errorf("an insertion-only divergence must skip rollback and migrate forward, got %q", msg)
	}
}
